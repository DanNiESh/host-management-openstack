/*
Copyright 2026.

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

package neutron_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac-project/host-management-openstack/internal/neutron"
)

func TestNeutron(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Neutron Suite")
}

var _ = Describe("Client", func() {
	Describe("NewClient", func() {
		Context("when OpenStack credentials are not configured", func() {
			var originalOSCloud, originalAuthURL string

			BeforeEach(func() {
				originalOSCloud = os.Getenv("OS_CLOUD")
				originalAuthURL = os.Getenv("OS_AUTH_URL")
				Expect(os.Unsetenv("OS_CLOUD")).To(Succeed())
				Expect(os.Unsetenv("OS_AUTH_URL")).To(Succeed())
			})

			AfterEach(func() {
				if originalOSCloud != "" {
					Expect(os.Setenv("OS_CLOUD", originalOSCloud)).To(Succeed())
				}
				if originalAuthURL != "" {
					Expect(os.Setenv("OS_AUTH_URL", originalAuthURL)).To(Succeed())
				}
			})

			It("should return an error when no credentials are available", func() {
				client, err := neutron.NewClient()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to create network client"))
				Expect(client).To(BeNil())
			})
		})

		Context("when OpenStack credentials are configured", func() {
			BeforeEach(func() {
				if !hasNeutronCredentials() {
					Skip("Skipping: OpenStack/Neutron credentials not configured in environment")
				}
			})

			It("should create a client successfully", func() {
				client, err := neutron.NewClient()
				Expect(err).NotTo(HaveOccurred())
				Expect(client).NotTo(BeNil())
				Expect(client.GetEndpoint()).NotTo(BeEmpty())
			})

			It("should have a valid HTTP(S) endpoint", func() {
				client, err := neutron.NewClient()
				Expect(err).NotTo(HaveOccurred())

				endpoint := client.GetEndpoint()
				Expect(endpoint).To(Or(
					HavePrefix("http://"),
					HavePrefix("https://"),
				))
			})
		})
	})
})

// hasNeutronCredentials checks if OpenStack/Neutron credentials are available.
func hasNeutronCredentials() bool {
	if os.Getenv("OS_CLOUD") != "" {
		return true
	}
	if os.Getenv("OS_AUTH_URL") != "" {
		return true
	}
	return false
}
