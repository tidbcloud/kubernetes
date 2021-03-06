// +build linux

/*
Copyright 2018 The Kubernetes Authors.

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

package kubelet

import (
	"fmt"

	"k8s.io/klog"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
)

// syncNetworkUtil ensures the network utility are present on host.
// Network util includes:
// 1. 	In nat table, KUBE-MARK-DROP rule to mark connections for dropping
// 	Marked connection will be drop on INPUT/OUTPUT Chain in filter table
// 2. 	In nat table, KUBE-MARK-MASQ rule to mark connections for SNAT
// 	Marked connection will get SNAT on POSTROUTING Chain in nat table
func (kl *Kubelet) syncNetworkUtil() {
	if kl.iptablesMasqueradeBit < 0 || kl.iptablesMasqueradeBit > 31 {
		klog.Errorf("invalid iptables-masquerade-bit %v not in [0, 31]", kl.iptablesMasqueradeBit)
		return
	}

	if kl.iptablesDropBit < 0 || kl.iptablesDropBit > 31 {
		klog.Errorf("invalid iptables-drop-bit %v not in [0, 31]", kl.iptablesDropBit)
		return
	}

	if kl.iptablesDropBit == kl.iptablesMasqueradeBit {
		klog.Errorf("iptables-masquerade-bit %v and iptables-drop-bit %v must be different", kl.iptablesMasqueradeBit, kl.iptablesDropBit)
		return
	}

	// Setup KUBE-MARK-DROP rules
	dropMark := getIPTablesMark(kl.iptablesDropBit)
	if _, err := kl.iptClient.EnsureChain(utiliptables.TableNAT, KubeMarkDropChain); err != nil {
		klog.Errorf("Failed to ensure that %s chain %s exists: %v", utiliptables.TableNAT, KubeMarkDropChain, err)
		return
	}
	if _, err := kl.iptClient.EnsureRule(utiliptables.Append, utiliptables.TableNAT, KubeMarkDropChain, "-j", "MARK", "--or-mark", dropMark); err != nil {
		klog.Errorf("Failed to ensure marking rule for %v: %v", KubeMarkDropChain, err)
		return
	}
	if _, err := kl.iptClient.EnsureChain(utiliptables.TableFilter, KubeFirewallChain); err != nil {
		klog.Errorf("Failed to ensure that %s chain %s exists: %v", utiliptables.TableFilter, KubeFirewallChain, err)
		return
	}
	if _, err := kl.iptClient.EnsureRule(utiliptables.Append, utiliptables.TableFilter, KubeFirewallChain,
		"-m", "comment", "--comment", "kubernetes firewall for dropping marked packets",
		"-m", "mark", "--mark", fmt.Sprintf("%s/%s", dropMark, dropMark),
		"-j", "DROP"); err != nil {
		klog.Errorf("Failed to ensure rule to drop packet marked by %v in %v chain %v: %v", KubeMarkDropChain, utiliptables.TableFilter, KubeFirewallChain, err)
		return
	}

	// drop all non-local packets to localhost if they're not part of an existing
	// forwarded connection. See #90259
	if !kl.iptClient.IsIpv6() { // ipv6 doesn't have this issue
		if _, err := kl.iptClient.EnsureRule(utiliptables.Append, utiliptables.TableFilter, KubeFirewallChain,
			"-m", "comment", "--comment", "block incoming localnet connections",
			"--dst", "127.0.0.0/8",
			"!", "--src", "127.0.0.0/8",
			"-m", "conntrack",
			"!", "--ctstate", "RELATED,ESTABLISHED,DNAT",
			"-j", "DROP"); err != nil {
			klog.Errorf("Failed to ensure rule to drop invalid localhost packets in %v chain %v: %v", utiliptables.TableFilter, KubeFirewallChain, err)
			return
		}
	}

	if _, err := kl.iptClient.EnsureRule(utiliptables.Prepend, utiliptables.TableFilter, utiliptables.ChainOutput, "-j", string(KubeFirewallChain)); err != nil {
		klog.Errorf("Failed to ensure that %s chain %s jumps to %s: %v", utiliptables.TableFilter, utiliptables.ChainOutput, KubeFirewallChain, err)
		return
	}
	if _, err := kl.iptClient.EnsureRule(utiliptables.Prepend, utiliptables.TableFilter, utiliptables.ChainInput, "-j", string(KubeFirewallChain)); err != nil {
		klog.Errorf("Failed to ensure that %s chain %s jumps to %s: %v", utiliptables.TableFilter, utiliptables.ChainInput, KubeFirewallChain, err)
		return
	}

	// Setup KUBE-MARK-MASQ rules
	masqueradeMark := getIPTablesMark(kl.iptablesMasqueradeBit)
	if _, err := kl.iptClient.EnsureChain(utiliptables.TableNAT, KubeMarkMasqChain); err != nil {
		klog.Errorf("Failed to ensure that %s chain %s exists: %v", utiliptables.TableNAT, KubeMarkMasqChain, err)
		return
	}
	if _, err := kl.iptClient.EnsureChain(utiliptables.TableNAT, KubePostroutingChain); err != nil {
		klog.Errorf("Failed to ensure that %s chain %s exists: %v", utiliptables.TableNAT, KubePostroutingChain, err)
		return
	}
	if _, err := kl.iptClient.EnsureRule(utiliptables.Append, utiliptables.TableNAT, KubeMarkMasqChain, "-j", "MARK", "--or-mark", masqueradeMark); err != nil {
		klog.Errorf("Failed to ensure marking rule for %v: %v", KubeMarkMasqChain, err)
		return
	}
	if _, err := kl.iptClient.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, utiliptables.ChainPostrouting,
		"-m", "comment", "--comment", "kubernetes postrouting rules", "-j", string(KubePostroutingChain)); err != nil {
		klog.Errorf("Failed to ensure that %s chain %s jumps to %s: %v", utiliptables.TableNAT, utiliptables.ChainPostrouting, KubePostroutingChain, err)
		return
	}

	// Set up KUBE-POSTROUTING to unmark and masquerade marked packets
	// NB: THIS MUST MATCH the corresponding code in the iptables and ipvs
	// modes of kube-proxy
	if _, err := kl.iptClient.EnsureRule(utiliptables.Append, utiliptables.TableNAT, KubePostroutingChain,
		"-m", "mark", "!", "--mark", fmt.Sprintf("%s/%s", masqueradeMark, masqueradeMark),
		"-j", "RETURN"); err != nil {
		klog.Errorf("Failed to ensure filtering rule for %v: %v", KubePostroutingChain, err)
		return
	}
	// Clear the mark to avoid re-masquerading if the packet re-traverses the network stack.
	// We know the mark bit is currently set so we can use --xor-mark to clear it (without needing
	// to Sprintf another bitmask).
	if _, err := kl.iptClient.EnsureRule(utiliptables.Append, utiliptables.TableNAT, KubePostroutingChain,
		"-j", "MARK", "--xor-mark", masqueradeMark); err != nil {
		klog.Errorf("Failed to ensure unmarking rule for %v: %v", KubePostroutingChain, err)
		return
	}
	masqRule := []string{
		"-m", "comment", "--comment", "kubernetes service traffic requiring SNAT",
		"-j", "MASQUERADE",
	}
	if kl.iptClient.HasRandomFully() {
		masqRule = append(masqRule, "--random-fully")
		klog.V(3).Info("Using `--random-fully` in the MASQUERADE rule for iptables")
	} else {
		klog.V(2).Info("Not using `--random-fully` in the MASQUERADE rule for iptables because the local version of iptables does not support it")
	}
	if _, err := kl.iptClient.EnsureRule(utiliptables.Append, utiliptables.TableNAT, KubePostroutingChain, masqRule...); err != nil {
		klog.Errorf("Failed to ensure SNAT rule for packets marked by %v in %v chain %v: %v", KubeMarkMasqChain, utiliptables.TableNAT, KubePostroutingChain, err)
		return
	}
}

// getIPTablesMark returns the fwmark given the bit
func getIPTablesMark(bit int) string {
	value := 1 << uint(bit)
	return fmt.Sprintf("%#08x", value)
}
