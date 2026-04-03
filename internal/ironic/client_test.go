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

package ironic_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"

	"github.com/osac-project/host-management-openstack/internal/ironic"
)

func TestIronic(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Ironic Suite")
}

var _ = Describe("TargetPowerState", func() {
	Describe("String", func() {
		It("should convert PowerOn to string correctly", func() {
			Expect(ironic.PowerOn.String()).To(Equal(string(nodes.PowerOn)))
			Expect(ironic.PowerOn.String()).To(Equal("power on"))
		})

		It("should convert PowerOff to string correctly", func() {
			Expect(ironic.PowerOff.String()).To(Equal(string(nodes.PowerOff)))
			Expect(ironic.PowerOff.String()).To(Equal("power off"))
		})

		It("should convert Rebooting to string correctly", func() {
			Expect(ironic.Rebooting.String()).To(Equal(string(nodes.Rebooting)))
			Expect(ironic.Rebooting.String()).To(Equal("rebooting"))
		})

		It("should convert SoftPowerOff to string correctly", func() {
			Expect(ironic.SoftPowerOff.String()).To(Equal(string(nodes.SoftPowerOff)))
			Expect(ironic.SoftPowerOff.String()).To(Equal("soft power off"))
		})

		It("should convert SoftRebooting to string correctly", func() {
			Expect(ironic.SoftRebooting.String()).To(Equal(string(nodes.SoftRebooting)))
			Expect(ironic.SoftRebooting.String()).To(Equal("soft rebooting"))
		})
	})

	Describe("Power state constants", func() {
		It("should have distinct values", func() {
			states := []string{
				ironic.PowerOn.String(),
				ironic.PowerOff.String(),
				ironic.Rebooting.String(),
				ironic.SoftPowerOff.String(),
				ironic.SoftRebooting.String(),
			}

			// Verify all states are unique
			uniqueStates := make(map[string]bool)
			for _, state := range states {
				Expect(uniqueStates[state]).To(BeFalse(), "duplicate power state: %s", state)
				uniqueStates[state] = true
			}
			Expect(uniqueStates).To(HaveLen(5))
		})
	})
})

var _ = Describe("Client", func() {
	Describe("NewClient", func() {
		Context("when OpenStack credentials are not configured", func() {
			var originalOSCloud, originalAuthURL string

			BeforeEach(func() {
				// Save and clear OpenStack environment variables
				originalOSCloud = os.Getenv("OS_CLOUD")
				originalAuthURL = os.Getenv("OS_AUTH_URL")
				Expect(os.Unsetenv("OS_CLOUD")).To(Succeed())
				Expect(os.Unsetenv("OS_AUTH_URL")).To(Succeed())
			})

			AfterEach(func() {
				// Restore original values
				if originalOSCloud != "" {
					Expect(os.Setenv("OS_CLOUD", originalOSCloud)).To(Succeed())
				}
				if originalAuthURL != "" {
					Expect(os.Setenv("OS_AUTH_URL", originalAuthURL)).To(Succeed())
				}
			})

			It("should return an error when no credentials are available", func() {
				client, err := ironic.NewClient()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to create baremetal client"))
				Expect(client).To(BeNil())
			})
		})

		Context("when OpenStack credentials are configured", func() {
			BeforeEach(func() {
				if !hasIronicCredentials() {
					Skip("Skipping: OpenStack/Ironic credentials not configured in environment")
				}
			})

			It("should create a client successfully", func() {
				client, err := ironic.NewClient()
				Expect(err).NotTo(HaveOccurred())
				Expect(client).NotTo(BeNil())
				Expect(client.GetEndpoint()).NotTo(BeEmpty())
			})

			It("should have a non-empty endpoint", func() {
				client, err := ironic.NewClient()
				Expect(err).NotTo(HaveOccurred())

				endpoint := client.GetEndpoint()
				Expect(endpoint).NotTo(BeEmpty())
			})
		})
	})

	Describe("GetEndpoint", func() {
		BeforeEach(func() {
			if !hasIronicCredentials() {
				Skip("Skipping: OpenStack/Ironic credentials not configured in environment")
			}
		})

		It("should return a valid HTTP(S) endpoint", func() {
			client, err := ironic.NewClient()
			Expect(err).NotTo(HaveOccurred())

			endpoint := client.GetEndpoint()
			Expect(endpoint).To(Or(
				HavePrefix("http://"),
				HavePrefix("https://"),
			))
		})
	})
})

// hasIronicCredentials checks if OpenStack/Ironic credentials are available
func hasIronicCredentials() bool {
	// Check for OS_CLOUD (most common for clouds.yaml)
	if os.Getenv("OS_CLOUD") != "" {
		return true
	}

	// Check for explicit auth URL (required for direct credentials)
	if os.Getenv("OS_AUTH_URL") != "" {
		return true
	}

	return false
}
