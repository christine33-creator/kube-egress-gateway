/*
Copyright 2024.

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

package manager

import (
	"testing"

	egressgatewayv1alpha1 "github.com/Azure/kube-egress-gateway/api/v1alpha1"
)

func TestGatewayProfileDetection(t *testing.T) {
	tests := []struct {
		name            string
		profile         egressgatewayv1alpha1.GatewayProfile
		expectedHasVMSS bool
		expectedHasVM   bool
	}{
		{
			name: "VMSS only deployment",
			profile: egressgatewayv1alpha1.GatewayProfile{
				VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
					PublicIpPrefixSize: 31,
					VmssName:           "gateway-vmss",
				},
			},
			expectedHasVMSS: true,
			expectedHasVM:   false,
		},
		{
			name: "Standalone VM only deployment",
			profile: egressgatewayv1alpha1.GatewayProfile{
				StandaloneVMProfile: &egressgatewayv1alpha1.GatewayStandaloneVMProfile{
					VMNames:            []string{"vm1", "vm2"},
					VMResourceGroup:    "test-rg",
					PublicIpPrefixSize: 31,
				},
			},
			expectedHasVMSS: false,
			expectedHasVM:   true,
		},
		{
			name: "Mixed deployment",
			profile: egressgatewayv1alpha1.GatewayProfile{
				VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
					PublicIpPrefixSize: 31,
					VmssName:           "gateway-vmss",
				},
				StandaloneVMProfile: &egressgatewayv1alpha1.GatewayStandaloneVMProfile{
					VMNames:            []string{"vm1", "vm2"},
					VMResourceGroup:    "test-rg",
					PublicIpPrefixSize: 31,
				},
			},
			expectedHasVMSS: true,
			expectedHasVM:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasVMSS := tt.profile.VmssProfile != nil
			if hasVMSS != tt.expectedHasVMSS {
				t.Errorf("expected hasVMSS %v, got %v", tt.expectedHasVMSS, hasVMSS)
			}

			hasVM := tt.profile.StandaloneVMProfile != nil
			if hasVM != tt.expectedHasVM {
				t.Errorf("expected hasStandaloneVM %v, got %v", tt.expectedHasVM, hasVM)
			}
		})
	}
}

func TestStaticGatewayConfigurationModes(t *testing.T) {
	tests := []struct {
		name            string
		spec            egressgatewayv1alpha1.StaticGatewayConfigurationSpec
		expectedHasVMSS bool
		expectedHasVM   bool
		expectedIsMixed bool
	}{
		{
			name: "VMSS configuration",
			spec: egressgatewayv1alpha1.StaticGatewayConfigurationSpec{
				GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
					VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
						VmssName:           "gateway-vmss",
						PublicIpPrefixSize: 31,
					},
				},
			},
			expectedHasVMSS: true,
			expectedHasVM:   false,
			expectedIsMixed: false,
		},
		{
			name: "Standalone VM configuration",
			spec: egressgatewayv1alpha1.StaticGatewayConfigurationSpec{
				GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
					StandaloneVMProfile: &egressgatewayv1alpha1.GatewayStandaloneVMProfile{
						VMNames:            []string{"vm1", "vm2"},
						VMResourceGroup:    "test-rg",
						PublicIpPrefixSize: 31,
					},
				},
			},
			expectedHasVMSS: false,
			expectedHasVM:   true,
			expectedIsMixed: false,
		},
		{
			name: "Mixed VMSS and Standalone VM configuration",
			spec: egressgatewayv1alpha1.StaticGatewayConfigurationSpec{
				GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
					VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
						VmssName:           "gateway-vmss",
						PublicIpPrefixSize: 31,
					},
					StandaloneVMProfile: &egressgatewayv1alpha1.GatewayStandaloneVMProfile{
						VMNames:            []string{"vm1", "vm2"},
						VMResourceGroup:    "test-rg",
						PublicIpPrefixSize: 31,
					},
				},
			},
			expectedHasVMSS: true,
			expectedHasVM:   true,
			expectedIsMixed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasVMSS := tt.spec.GatewayProfile.VmssProfile != nil
			if hasVMSS != tt.expectedHasVMSS {
				t.Errorf("expected hasVMSS %v, got %v", tt.expectedHasVMSS, hasVMSS)
			}

			hasVM := tt.spec.GatewayProfile.StandaloneVMProfile != nil
			if hasVM != tt.expectedHasVM {
				t.Errorf("expected hasStandaloneVM %v, got %v", tt.expectedHasVM, hasVM)
			}

			isMixed := hasVMSS && hasVM
			if isMixed != tt.expectedIsMixed {
				t.Errorf("expected isMixed %v, got %v", tt.expectedIsMixed, isMixed)
			}
		})
	}
}

func TestGatewayVMConfigurationModes(t *testing.T) {
	tests := []struct {
		name            string
		spec            egressgatewayv1alpha1.GatewayVMConfigurationSpec
		expectedHasVMSS bool
		expectedHasVM   bool
		expectedIsMixed bool
	}{
		{
			name: "VMSS configuration via GatewayProfile",
			spec: egressgatewayv1alpha1.GatewayVMConfigurationSpec{
				GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
					VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
						VmssName:           "gateway-vmss",
						PublicIpPrefixSize: 31,
					},
				},
			},
			expectedHasVMSS: true,
			expectedHasVM:   false,
			expectedIsMixed: false,
		},
		{
			name: "Mixed deployment via GatewayProfile",
			spec: egressgatewayv1alpha1.GatewayVMConfigurationSpec{
				GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
					VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
						VmssName:           "gateway-vmss",
						PublicIpPrefixSize: 31,
					},
					StandaloneVMProfile: &egressgatewayv1alpha1.GatewayStandaloneVMProfile{
						VMNames:            []string{"vm1", "vm2"},
						VMResourceGroup:    "test-rg",
						PublicIpPrefixSize: 31,
					},
				},
			},
			expectedHasVMSS: true,
			expectedHasVM:   true,
			expectedIsMixed: true,
		},
		{
			name: "Legacy VMSS configuration via deprecated field",
			spec: egressgatewayv1alpha1.GatewayVMConfigurationSpec{
				GatewayVmssProfile: egressgatewayv1alpha1.GatewayVmssProfile{
					VmssName:           "gateway-vmss",
					PublicIpPrefixSize: 31,
				},
			},
			expectedHasVMSS: true,
			expectedHasVM:   false,
			expectedIsMixed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasVMSS := tt.spec.GatewayProfile.VmssProfile != nil ||
				tt.spec.GatewayVmssProfile.VmssName != ""
			if hasVMSS != tt.expectedHasVMSS {
				t.Errorf("expected hasVMSS %v, got %v", tt.expectedHasVMSS, hasVMSS)
			}

			hasVM := tt.spec.GatewayProfile.StandaloneVMProfile != nil
			if hasVM != tt.expectedHasVM {
				t.Errorf("expected hasStandaloneVM %v, got %v", tt.expectedHasVM, hasVM)
			}

			isMixed := hasVMSS && hasVM
			if isMixed != tt.expectedIsMixed {
				t.Errorf("expected isMixed %v, got %v", tt.expectedIsMixed, isMixed)
			}
		})
	}
}

// Test helper functions that would be used in the main controller

func TestIsStandaloneVMMode(t *testing.T) {
	tests := []struct {
		name     string
		config   *egressgatewayv1alpha1.StaticGatewayConfiguration
		expected bool
	}{
		{
			name: "Standalone VM mode",
			config: &egressgatewayv1alpha1.StaticGatewayConfiguration{
				Spec: egressgatewayv1alpha1.StaticGatewayConfigurationSpec{
					GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
						StandaloneVMProfile: &egressgatewayv1alpha1.GatewayStandaloneVMProfile{
							VMNames: []string{"vm1"},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "VMSS mode",
			config: &egressgatewayv1alpha1.StaticGatewayConfiguration{
				Spec: egressgatewayv1alpha1.StaticGatewayConfigurationSpec{
					GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
						VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
							VmssName: "gateway-vmss",
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "Mixed mode - should return false for isStandaloneVMMode",
			config: &egressgatewayv1alpha1.StaticGatewayConfiguration{
				Spec: egressgatewayv1alpha1.StaticGatewayConfigurationSpec{
					GatewayProfile: egressgatewayv1alpha1.GatewayProfile{
						VmssProfile: &egressgatewayv1alpha1.GatewayVmssProfile{
							VmssName: "gateway-vmss",
						},
						StandaloneVMProfile: &egressgatewayv1alpha1.GatewayStandaloneVMProfile{
							VMNames: []string{"vm1"},
						},
					},
				},
			},
			expected: false, // Mixed mode is not pure standalone mode
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isStandaloneVMMode(tt.config)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// Helper function that would be implemented in the main controller
func isStandaloneVMMode(config *egressgatewayv1alpha1.StaticGatewayConfiguration) bool {
	if config == nil {
		return false
	}

	hasStandaloneVM := config.Spec.GatewayProfile.StandaloneVMProfile != nil
	hasVMSS := config.Spec.GatewayProfile.VmssProfile != nil ||
		config.Spec.GatewayVmssProfile.VmssName != ""

	// Pure standalone mode: has standalone VMs but no VMSS
	return hasStandaloneVM && !hasVMSS
}
