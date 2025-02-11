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
	"bytes"
)

// CAContentProvider provides ca bundle byte content
type CAContentProvider interface {
	// Name is just an identifier
	Name() string
	// CurrentCABundleContent provides ca bundle byte content.  Errors can be contained to the controllers initializing
	// the value.  By the time you get here, you should always be returning a value that won't fail.
	CurrentCABundleContent() []byte
}

// dynamicCertificateContent holds the content that overrides the baseTLSConfig
// TODO add the serving certs to this struct
type dynamicCertificateContent struct {
	// clientCA holds the content for the clientCA bundle
	clientCA caBundleContent
}

// caBundleContent holds the content for the clientCA bundle.  Wrapping the bytes makes the Equals work nicely with the
// method receiver.
type caBundleContent struct {
	caBundle []byte
}

func (c *dynamicCertificateContent) Equal(rhs *dynamicCertificateContent) bool {
	if c == nil || rhs == nil {
		return c == rhs
	}

	if !c.clientCA.Equal(&rhs.clientCA) {
		return false
	}

	return true
}

func (c *caBundleContent) Equal(rhs *caBundleContent) bool {
	if c == nil || rhs == nil {
		return c == rhs
	}

	return bytes.Equal(c.caBundle, rhs.caBundle)
}
