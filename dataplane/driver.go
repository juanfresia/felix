// Copyright (c) 2020-2021 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build !windows

package dataplane

import (
	"math/bits"
	"net"
	"net/http"
	"os/exec"
	"runtime/debug"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/kubernetes"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	apiv3 "github.com/projectcalico/api/pkg/apis/projectcalico/v3"
	"github.com/projectcalico/felix/aws"
	"github.com/projectcalico/felix/bpf"
	"github.com/projectcalico/felix/bpf/conntrack"
	"github.com/projectcalico/felix/bpf/tc"
	"github.com/projectcalico/felix/config"
	extdataplane "github.com/projectcalico/felix/dataplane/external"
	"github.com/projectcalico/felix/dataplane/inactive"
	intdataplane "github.com/projectcalico/felix/dataplane/linux"
	"github.com/projectcalico/felix/idalloc"
	"github.com/projectcalico/felix/ifacemonitor"
	"github.com/projectcalico/felix/ipsets"
	"github.com/projectcalico/felix/logutils"
	"github.com/projectcalico/felix/markbits"
	"github.com/projectcalico/felix/rules"
	"github.com/projectcalico/felix/wireguard"
	"github.com/projectcalico/libcalico-go/lib/health"
)

func StartDataplaneDriver(configParams *config.Config,
	healthAggregator *health.HealthAggregator,
	configChangedRestartCallback func(),
	fatalErrorCallback func(error),
	k8sClientSet *kubernetes.Clientset) (DataplaneDriver, *exec.Cmd) {

	if !configParams.IsLeader() {
		// Return an inactive dataplane, since we're not the leader.
		log.Info("Not the leader, using an inactive dataplane")
		return &inactive.InactiveDataplane{}, nil
	}

	if configParams.UseInternalDataplaneDriver {
		log.Info("Using internal (linux) dataplane driver.")
		// If kube ipvs interface is present, enable ipvs support.  In BPF mode, we bypass kube-proxy so IPVS
		// is irrelevant.
		kubeIPVSSupportEnabled := false
		if ifacemonitor.IsInterfacePresent(intdataplane.KubeIPVSInterface) {
			if configParams.BPFEnabled {
				log.Info("kube-proxy IPVS device found but we're in BPF mode, ignoring.")
			} else {
				kubeIPVSSupportEnabled = true
				log.Info("Kube-proxy in ipvs mode, enabling felix kube-proxy ipvs support.")
			}
		}
		if configChangedRestartCallback == nil || fatalErrorCallback == nil {
			log.Panic("Starting dataplane with nil callback func.")
		}

		allowedMarkBits := configParams.IptablesMarkMask
		if configParams.BPFEnabled {
			// In BPF mode, the BPF programs use mark bits that are not configurable.  Make sure that those
			// bits are covered by our allowed mask.
			if allowedMarkBits&tc.MarksMask != tc.MarksMask {
				log.WithFields(log.Fields{
					"Name":            "felix-iptables",
					"MarkMask":        allowedMarkBits,
					"RequiredBPFBits": tc.MarksMask,
				}).Panic("IptablesMarkMask doesn't cover bits that are used (unconditionally) by eBPF mode.")
			}
			allowedMarkBits ^= allowedMarkBits & tc.MarksMask
			log.WithField("updatedBits", allowedMarkBits).Info(
				"Removed BPF program bits from available mark bits.")
		}

		markBitsManager := markbits.NewMarkBitsManager(allowedMarkBits, "felix-iptables")

		// Allocate mark bits; only the accept, scratch-0 and Wireguard bits are used in BPF mode so we
		// avoid allocating the others to minimize the number of bits in use.

		// The accept bit is a long-lived bit used to communicate between chains.
		var markAccept, markPass, markScratch0, markScratch1, markWireguard, markEndpointNonCaliEndpoint uint32
		markAccept, _ = markBitsManager.NextSingleBitMark()
		if !configParams.BPFEnabled {
			// The pass bit is used to communicate from a policy chain up to the endpoint chain.
			markPass, _ = markBitsManager.NextSingleBitMark()
		}

		// Scratch bits are short-lived bits used for calculating multi-rule results.
		markScratch0, _ = markBitsManager.NextSingleBitMark()
		if !configParams.BPFEnabled {
			markScratch1, _ = markBitsManager.NextSingleBitMark()
		}
		if configParams.WireguardEnabled {
			log.Info("Wireguard enabled, allocating a mark bit")
			markWireguard, _ = markBitsManager.NextSingleBitMark()
			if markWireguard == 0 {
				log.WithFields(log.Fields{
					"Name":     "felix-iptables",
					"MarkMask": allowedMarkBits,
				}).Panic("Failed to allocate a mark bit for wireguard, not enough mark bits available.")
			}
		}

		// markPass and the scratch-1 bits are only used in iptables mode.
		if markAccept == 0 || markScratch0 == 0 || !configParams.BPFEnabled && (markPass == 0 || markScratch1 == 0) {
			log.WithFields(log.Fields{
				"Name":     "felix-iptables",
				"MarkMask": allowedMarkBits,
			}).Panic("Not enough mark bits available.")
		}

		// Mark bits for endpoint mark. Currently Felix takes the rest bits from mask available for use.
		markEndpointMark, allocated := markBitsManager.NextBlockBitsMark(markBitsManager.AvailableMarkBitCount())
		if kubeIPVSSupportEnabled {
			if allocated == 0 {
				log.WithFields(log.Fields{
					"Name":     "felix-iptables",
					"MarkMask": allowedMarkBits,
				}).Panic("Not enough mark bits available for endpoint mark.")
			}
			// Take lowest bit position (position 1) from endpoint mark mask reserved for non-calico endpoint.
			markEndpointNonCaliEndpoint = uint32(1) << uint(bits.TrailingZeros32(markEndpointMark))
		}
		log.WithFields(log.Fields{
			"acceptMark":          markAccept,
			"passMark":            markPass,
			"scratch0Mark":        markScratch0,
			"scratch1Mark":        markScratch1,
			"endpointMark":        markEndpointMark,
			"endpointMarkNonCali": markEndpointNonCaliEndpoint,
		}).Info("Calculated iptables mark bits")

		// Create a routing table manager. There are certain components that should take specific indices in the range
		// to simplify table tidy-up.
		routeTableIndexAllocator := idalloc.NewIndexAllocator(configParams.RouteTableRange)

		// Always allocate the wireguard table index (even when not enabled). This ensures we can tidy up entries
		// if wireguard is disabled after being previously enabled.
		var wireguardEnabled bool
		var wireguardTableIndex int
		if idx, err := routeTableIndexAllocator.GrabIndex(); err == nil {
			log.Debugf("Assigned wireguard table index: %d", idx)
			wireguardEnabled = configParams.WireguardEnabled
			wireguardTableIndex = idx
		} else {
			log.WithError(err).Warning("Unable to assign table index for wireguard")
		}

		// If wireguard is enabled, update the failsafe ports to include the wireguard port.
		failsafeInboundHostPorts := configParams.FailsafeInboundHostPorts
		failsafeOutboundHostPorts := configParams.FailsafeOutboundHostPorts
		if configParams.WireguardEnabled {
			var found = false
			for _, i := range failsafeInboundHostPorts {
				if i.Port == uint16(configParams.WireguardListeningPort) && i.Protocol == "udp" {
					log.WithFields(log.Fields{
						"net":      i.Net,
						"port":     i.Port,
						"protocol": i.Protocol,
					}).Debug("FailsafeInboundHostPorts is already configured for wireguard")
					found = true
					break
				}
			}
			if !found {
				failsafeInboundHostPorts = make([]config.ProtoPort, len(configParams.FailsafeInboundHostPorts)+1)
				copy(failsafeInboundHostPorts, configParams.FailsafeInboundHostPorts)
				log.Debug("Adding permissive FailsafeInboundHostPorts for wireguard")
				failsafeInboundHostPorts[len(configParams.FailsafeInboundHostPorts)] = config.ProtoPort{
					Port:     uint16(configParams.WireguardListeningPort),
					Protocol: "udp",
				}
			}

			found = false
			for _, i := range failsafeOutboundHostPorts {
				if i.Port == uint16(configParams.WireguardListeningPort) && i.Protocol == "udp" {
					log.WithFields(log.Fields{
						"net":      i.Net,
						"port":     i.Port,
						"protocol": i.Protocol,
					}).Debug("FailsafeOutboundHostPorts is already configured for wireguard")
					found = true
					break
				}
			}
			if !found {
				failsafeOutboundHostPorts = make([]config.ProtoPort, len(configParams.FailsafeOutboundHostPorts)+1)
				copy(failsafeOutboundHostPorts, configParams.FailsafeOutboundHostPorts)
				log.Debug("Adding permissive FailsafeOutboundHostPorts for wireguard")
				failsafeOutboundHostPorts[len(configParams.FailsafeOutboundHostPorts)] = config.ProtoPort{
					Port:     uint16(configParams.WireguardListeningPort),
					Protocol: "udp",
				}
			}
		}

		dpConfig := intdataplane.Config{
			Hostname: configParams.FelixHostname,
			IfaceMonitorConfig: ifacemonitor.Config{
				InterfaceExcludes: configParams.InterfaceExclude,
				ResyncInterval:    configParams.InterfaceRefreshInterval,
			},
			RulesConfig: rules.Config{
				WorkloadIfacePrefixes: configParams.InterfacePrefixes(),

				IPSetConfigV4: ipsets.NewIPVersionConfig(
					ipsets.IPFamilyV4,
					rules.IPSetNamePrefix,
					rules.AllHistoricIPSetNamePrefixes,
					rules.LegacyV4IPSetNames,
				),
				IPSetConfigV6: ipsets.NewIPVersionConfig(
					ipsets.IPFamilyV6,
					rules.IPSetNamePrefix,
					rules.AllHistoricIPSetNamePrefixes,
					nil,
				),

				KubeNodePortRanges:     configParams.KubeNodePortRanges,
				KubeIPVSSupportEnabled: kubeIPVSSupportEnabled,

				OpenStackSpecialCasesEnabled: configParams.OpenstackActive(),
				OpenStackMetadataIP:          net.ParseIP(configParams.MetadataAddr),
				OpenStackMetadataPort:        uint16(configParams.MetadataPort),

				IptablesMarkAccept:          markAccept,
				IptablesMarkPass:            markPass,
				IptablesMarkScratch0:        markScratch0,
				IptablesMarkScratch1:        markScratch1,
				IptablesMarkEndpoint:        markEndpointMark,
				IptablesMarkNonCaliEndpoint: markEndpointNonCaliEndpoint,

				VXLANEnabled: configParams.VXLANEnabled,
				VXLANPort:    configParams.VXLANPort,
				VXLANVNI:     configParams.VXLANVNI,

				IPIPEnabled:        configParams.IpInIpEnabled,
				IPIPTunnelAddress:  configParams.IpInIpTunnelAddr,
				VXLANTunnelAddress: configParams.IPv4VXLANTunnelAddr,

				AllowVXLANPacketsFromWorkloads: configParams.AllowVXLANPacketsFromWorkloads,
				AllowIPIPPacketsFromWorkloads:  configParams.AllowIPIPPacketsFromWorkloads,

				WireguardEnabled:            configParams.WireguardEnabled,
				WireguardInterfaceName:      configParams.WireguardInterfaceName,
				WireguardIptablesMark:       markWireguard,
				WireguardListeningPort:      configParams.WireguardListeningPort,
				WireguardEncryptHostTraffic: configParams.WireguardHostEncryptionEnabled,
				RouteSource:                 configParams.RouteSource,

				IptablesLogPrefix:         configParams.LogPrefix,
				EndpointToHostAction:      configParams.DefaultEndpointToHostAction,
				IptablesFilterAllowAction: configParams.IptablesFilterAllowAction,
				IptablesMangleAllowAction: configParams.IptablesMangleAllowAction,

				FailsafeInboundHostPorts:  failsafeInboundHostPorts,
				FailsafeOutboundHostPorts: failsafeOutboundHostPorts,

				DisableConntrackInvalid: configParams.DisableConntrackInvalidCheck,

				NATPortRange:                       configParams.NATPortRange,
				IptablesNATOutgoingInterfaceFilter: configParams.IptablesNATOutgoingInterfaceFilter,
				NATOutgoingAddress:                 configParams.NATOutgoingAddress,
				BPFEnabled:                         configParams.BPFEnabled,
				ServiceLoopPrevention:              configParams.ServiceLoopPrevention,
			},
			Wireguard: wireguard.Config{
				Enabled:             wireguardEnabled,
				ListeningPort:       configParams.WireguardListeningPort,
				FirewallMark:        int(markWireguard),
				RoutingRulePriority: configParams.WireguardRoutingRulePriority,
				RoutingTableIndex:   wireguardTableIndex,
				InterfaceName:       configParams.WireguardInterfaceName,
				MTU:                 configParams.WireguardMTU,
				RouteSource:         configParams.RouteSource,
				EncryptHostTraffic:  configParams.WireguardHostEncryptionEnabled,
			},
			IPIPMTU:                        configParams.IpInIpMtu,
			VXLANMTU:                       configParams.VXLANMTU,
			VXLANPort:                      configParams.VXLANPort,
			IptablesBackend:                configParams.IptablesBackend,
			IptablesRefreshInterval:        configParams.IptablesRefreshInterval,
			RouteRefreshInterval:           configParams.RouteRefreshInterval,
			DeviceRouteSourceAddress:       configParams.DeviceRouteSourceAddress,
			DeviceRouteProtocol:            netlink.RouteProtocol(configParams.DeviceRouteProtocol),
			RemoveExternalRoutes:           configParams.RemoveExternalRoutes,
			IPSetsRefreshInterval:          configParams.IpsetsRefreshInterval,
			IptablesPostWriteCheckInterval: configParams.IptablesPostWriteCheckIntervalSecs,
			IptablesInsertMode:             configParams.ChainInsertMode,
			IptablesLockFilePath:           configParams.IptablesLockFilePath,
			IptablesLockTimeout:            configParams.IptablesLockTimeoutSecs,
			IptablesLockProbeInterval:      configParams.IptablesLockProbeIntervalMillis,
			MaxIPSetSize:                   configParams.MaxIpsetSize,
			IPv6Enabled:                    configParams.Ipv6Support,
			StatusReportingInterval:        configParams.ReportingIntervalSecs,
			XDPRefreshInterval:             configParams.XDPRefreshInterval,

			NetlinkTimeout: configParams.NetlinkTimeoutSecs,

			ConfigChangedRestartCallback: configChangedRestartCallback,
			FatalErrorRestartCallback:    fatalErrorCallback,

			PostInSyncCallback: func() {
				// The initial resync uses a lot of scratch space so now is
				// a good time to force a GC and return any RAM that we can.
				debug.FreeOSMemory()

				if configParams.DebugMemoryProfilePath == "" {
					return
				}
				logutils.DumpHeapMemoryProfile(configParams.DebugMemoryProfilePath)
			},
			HealthAggregator:                   healthAggregator,
			DebugSimulateDataplaneHangAfter:    configParams.DebugSimulateDataplaneHangAfter,
			ExternalNodesCidrs:                 configParams.ExternalNodesCIDRList,
			SidecarAccelerationEnabled:         configParams.SidecarAccelerationEnabled,
			BPFEnabled:                         configParams.BPFEnabled,
			BPFDisableUnprivileged:             configParams.BPFDisableUnprivileged,
			BPFConnTimeLBEnabled:               configParams.BPFConnectTimeLoadBalancingEnabled,
			BPFKubeProxyIptablesCleanupEnabled: configParams.BPFKubeProxyIptablesCleanupEnabled,
			BPFLogLevel:                        configParams.BPFLogLevel,
			BPFExtToServiceConnmark:            configParams.BPFExtToServiceConnmark,
			BPFDataIfacePattern:                configParams.BPFDataIfacePattern,
			BPFCgroupV2:                        configParams.DebugBPFCgroupV2,
			BPFMapRepin:                        configParams.DebugBPFMapRepinEnabled,
			KubeProxyMinSyncPeriod:             configParams.BPFKubeProxyMinSyncPeriod,
			KubeProxyEndpointSlicesEnabled:     configParams.BPFKubeProxyEndpointSlicesEnabled,
			XDPEnabled:                         configParams.XDPEnabled,
			XDPAllowGeneric:                    configParams.GenericXDPEnabled,
			BPFConntrackTimeouts:               conntrack.DefaultTimeouts(), // FIXME make timeouts configurable
			RouteTableManager:                  routeTableIndexAllocator,
			MTUIfacePattern:                    configParams.MTUIfacePattern,

			KubeClientSet: k8sClientSet,

			FeatureDetectOverrides: configParams.FeatureDetectOverride,

			RouteSource: configParams.RouteSource,

			KubernetesProvider: configParams.KubernetesProvider(),
		}

		if configParams.BPFExternalServiceMode == "dsr" {
			dpConfig.BPFNodePortDSREnabled = true
		}

		intDP := intdataplane.NewIntDataplaneDriver(dpConfig)
		intDP.Start()

		// Set source-destination-check on AWS EC2 instance.
		if configParams.AWSSrcDstCheck != string(apiv3.AWSSrcDstCheckOptionDoNothing) {
			c := &clock.RealClock{}
			updater := aws.NewEC2SrcDstCheckUpdater()
			go aws.WaitForEC2SrcDstCheckUpdate(configParams.AWSSrcDstCheck, healthAggregator, updater, c)
		}

		return intDP, nil
	} else {
		log.WithField("driver", configParams.DataplaneDriver).Info(
			"Using external dataplane driver.")

		return extdataplane.StartExtDataplaneDriver(configParams.DataplaneDriver)
	}
}

func SupportsBPF() error {
	return bpf.SupportsBPFDataplane()
}

func ServePrometheusMetrics(configParams *config.Config) {
	log.WithFields(log.Fields{
		"host": configParams.PrometheusMetricsHost,
		"port": configParams.PrometheusMetricsPort,
	}).Info("Starting prometheus metrics endpoint")
	if configParams.PrometheusGoMetricsEnabled && configParams.PrometheusProcessMetricsEnabled && configParams.PrometheusWireGuardMetricsEnabled {
		log.Info("Including Golang, Process and WireGuard metrics")
	} else {
		if !configParams.PrometheusGoMetricsEnabled {
			log.Info("Discarding Golang metrics")
			prometheus.Unregister(prometheus.NewGoCollector())
		}
		if !configParams.PrometheusProcessMetricsEnabled {
			log.Info("Discarding process metrics")
			prometheus.Unregister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
		}
		if !configParams.PrometheusWireGuardMetricsEnabled {
			log.Info("Discarding WireGuard metrics")
			prometheus.Unregister(wireguard.MustNewWireguardMetrics())
		}
	}
	http.Handle("/metrics", promhttp.Handler())
	addr := net.JoinHostPort(configParams.PrometheusMetricsHost, strconv.Itoa(configParams.PrometheusMetricsPort))
	for {
		err := http.ListenAndServe(addr, nil)
		log.WithError(err).Error(
			"Prometheus metrics endpoint failed, trying to restart it...")
		time.Sleep(1 * time.Second)
	}
}
