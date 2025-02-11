/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dynamiccertificates

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const workItemKey = "key"

// DynamicServingCertificateController dynamically loads certificates and provides a golang tls compatible dynamic GetCertificate func.
type DynamicServingCertificateController struct {
	// baseTLSConfig is the static portion of the tlsConfig for serving to clients.  It is copied and the copy is mutated
	// based on the dynamic cert state.
	baseTLSConfig tls.Config

	// clientCA provides the very latest content of the ca bundle
	clientCA CAContentProvider

	// currentlyServedContent holds the original bytes that we are serving. This is used to decide if we need to set a
	// new atomic value. The types used for efficient TLSConfig preclude using the processed value.
	currentlyServedContent *dynamicCertificateContent
	// currentServingTLSConfig holds a *tls.Config that will be used to serve requests
	currentServingTLSConfig atomic.Value

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue         workqueue.RateLimitingInterface
	eventRecorder events.EventRecorder
}

// NewDynamicServingCertificateController returns a controller that can be used to keep a TLSConfig up to date.
func NewDynamicServingCertificateController(
	baseTLSConfig tls.Config,
	clientCA CAContentProvider,
	eventRecorder events.EventRecorder,
) *DynamicServingCertificateController {
	c := &DynamicServingCertificateController{
		baseTLSConfig: baseTLSConfig,
		clientCA:      clientCA,

		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DynamicServingCertificateController"),
		eventRecorder: eventRecorder,
	}

	return c
}

// GetConfigForClient is an implementation of tls.Config.GetConfigForClient
func (c *DynamicServingCertificateController) GetConfigForClient(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
	uncastObj := c.currentServingTLSConfig.Load()
	if uncastObj == nil {
		return nil, errors.New("dynamiccertificates: configuration not ready")
	}
	tlsConfig, ok := uncastObj.(*tls.Config)
	if !ok {
		return nil, errors.New("dynamiccertificates: unexpected config type")
	}

	return tlsConfig.Clone(), nil
}

// newTLSContent determines the next set of content for overriding the baseTLSConfig.
func (c *DynamicServingCertificateController) newTLSContent() (*dynamicCertificateContent, error) {
	newContent := &dynamicCertificateContent{}

	currClientCABundle := c.clientCA.CurrentCABundleContent()
	// don't remove all content.  The value was configured at one time, so continue using that.
	// Errors reading content can be reported by lower level controllers.
	if len(currClientCABundle) == 0 {
		return nil, fmt.Errorf("not loading an empty client ca bundle from %q", c.clientCA.Name())
	}
	newContent.clientCA = caBundleContent{caBundle: currClientCABundle}

	return newContent, nil
}

// syncCerts gets newTLSContent, if it has changed from the existing, the content is parsed and stored for usage in
// GetConfigForClient.
func (c *DynamicServingCertificateController) syncCerts() error {
	newContent, err := c.newTLSContent()
	if err != nil {
		return err
	}
	// if the content is the same as what we currently have, we can simply skip it.  This works because we are single
	// threaded.  If you ever make this multi-threaded, add a lock.
	if newContent.Equal(c.currentlyServedContent) {
		return nil
	}

	// parse new content to add to TLSConfig
	newClientCAPool := x509.NewCertPool()
	if len(newContent.clientCA.caBundle) > 0 {
		newClientCAs, err := cert.ParseCertsPEM(newContent.clientCA.caBundle)
		if err != nil {
			return fmt.Errorf("unable to load client CA file: %v", err)
		}
		for i, cert := range newClientCAs {
			klog.V(2).Infof("loaded client CA [%d/%q]: %s", i, c.clientCA.Name(), GetHumanCertDetail(cert))
			if c.eventRecorder != nil {
				c.eventRecorder.Eventf(nil, nil, v1.EventTypeWarning, "TLSConfigChanged", "CACertificateReload", "loaded client CA [%d/%q]: %s", i, c.clientCA.Name(), GetHumanCertDetail(cert))
			}

			newClientCAPool.AddCert(cert)
		}
	}

	// make a copy and override the dynamic pieces which have changed.
	newTLSConfigCopy := c.baseTLSConfig.Clone()
	newTLSConfigCopy.ClientCAs = newClientCAPool

	// store new values of content for serving.
	c.currentServingTLSConfig.Store(newTLSConfigCopy)
	c.currentlyServedContent = newContent // this is single threaded, so we have no locking issue

	return nil
}

// RunOnce runs a single sync step to ensure that we have a valid starting configuration.
func (c *DynamicServingCertificateController) RunOnce() error {
	return c.syncCerts()
}

// Run starts the kube-apiserver and blocks until stopCh is closed.
func (c *DynamicServingCertificateController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting DynamicServingCertificateController")
	defer klog.Infof("Shutting down DynamicServingCertificateController")

	// synchronously load once.  We will trigger again, so ignoring any error is fine
	_ = c.RunOnce()

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	// start timer that rechecks every minute, just in case.  this also serves to prime the controller quickly.
	go wait.Until(func() {
		c.Enqueue()
	}, 1*time.Minute, stopCh)

	<-stopCh
}

func (c *DynamicServingCertificateController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *DynamicServingCertificateController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.syncCerts()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// Enqueue a method to allow separate control loops to cause the certificate controller to trigger and read content.
func (c *DynamicServingCertificateController) Enqueue() {
	c.queue.Add(workItemKey)
}
