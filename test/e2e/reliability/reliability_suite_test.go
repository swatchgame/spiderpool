// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0
package reliability_test

import (
	e2e "github.com/spidernet-io/e2eframework/framework"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	spiderpool "github.com/spidernet-io/spiderpool/pkg/k8s/apis/v1"
)

func TestReliability(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Reliability Suite")
}

var frame *e2e.Framework

var _ = BeforeSuite(func() {
	defer GinkgoRecover()
	var e error
	frame, e = e2e.NewFramework(GinkgoT(),[]
	{
		spiderpool.AddToScheme
	} )
	Expect(e).NotTo(HaveOccurred())
})
