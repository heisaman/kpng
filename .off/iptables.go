package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	"github.com/cespare/xxhash"
	"github.com/golang/protobuf/proto"

	"github.com/mcluseau/kube-proxy2/pkg/api/localnetv1"
)

func localnetExtIptables() {
	forwardChain := *iptChainPrefix + "forward"
	dnatChain := *iptChainPrefix + "DNAT"
	snatChain := *iptChainPrefix + "SNAT"

	var (
		rev      uint64
		prevHash uint64
	)

	for {
		snap := sepStore.Next(rev)
		rev = snap.Rev()

		log.Print("ext-iptables: at rev ", rev)

		seps := make(SEps, 0)

		for kv := range snap.Iterate(func() proto.Message { return &localnetv1.ServiceEndpoints{} }) {
			if kv.Err != nil {
				log.Fatal("failed to iterate snapshot: ", kv.Err)
			}

			sep := kv.Value.(*localnetv1.ServiceEndpoints)

			if *extLBsOnly && sep.Type != "LoadBalancer" {
				// only process LBs
				continue
			}

			if sep.IPs.ExternalIPs.Len() == 0 {
				// filter out services without external IPs
				continue
			}

			seps = append(seps, sep)
		}

		ipt := &bytes.Buffer{}

		fmt.Fprint(ipt, "*filter\n")
		fmt.Fprint(ipt, ":", forwardChain, " -\n")
		for _, sep := range seps {
			key := path.Join(sep.Namespace, sep.Name)
			for _, ip := range sep.IPs.EndpointIPs.GetAsStrings() {
				for _, port := range sep.Ports {
					proto := strings.ToLower(port.Protocol.String())

					fmt.Fprintf(ipt, "-A %s -d %s -j ACCEPT -m %s -p %s --dport %d %s\n",
						forwardChain, ip, proto, proto, port.TargetPort,
						iptCommentf("%s: %s:%d -> %d", key, proto, port.Port, port.TargetPort))
				}
			}
		}

		fmt.Fprint(ipt, "COMMIT\n")

		// NAT chain
		fmt.Fprint(ipt, "*nat\n")
		fmt.Fprint(ipt, ":", dnatChain, " -\n")
		fmt.Fprint(ipt, ":", snatChain, " -\n")

		// DNAT rules
		for _, sep := range seps {
			key := path.Join(sep.Namespace, sep.Name)

			epCount := sep.IPs.EndpointIPs.Len()

			if epCount == 0 {
				continue
			}

			epIPs := sep.IPs.EndpointIPs.GetAsStrings()

			for _, extIP := range sep.IPs.ExternalIPs.GetAsStrings() {
				for i, ip := range epIPs {
					rndProba := iptRandom(i, epCount)

					for _, port := range sep.Ports {
						proto := strings.ToLower(port.Protocol.String())

						fmt.Fprintf(ipt, "-A %s -d %s -m %s -p %s --dport %d -j DNAT --to-destination %s:%d %s %s\n",
							dnatChain, extIP, proto, proto, port.Port, ip, port.TargetPort, rndProba,
							iptCommentf("%s: %s:%d -> %s:%d", key, extIP, port.Port, ip, port.TargetPort))
					}
				}
			}
		}

		// SNAT rules
		revExt := map[string]string{}
		revExtKey := map[string]string{}
		for _, sep := range seps {
			key := path.Join(sep.Namespace, sep.Name)

			if sep.IPs.EndpointIPs.Len() == 0 {
				continue
			}

			// use the first external IP
			extIPs := sep.IPs.EndpointIPs.GetAsStrings()
			extIP := extIPs[0]

			for _, ip := range extIPs {
				if revExt[ip] == "" || extIP < revExt[ip] {
					revExt[ip] = extIP
					revExtKey[ip] = key
				}
			}
		}

		epIPs := make([]string, 0, len(revExt))
		for epIP := range revExt {
			epIPs = append(epIPs, epIP)
		}

		sort.Strings(epIPs)

		for _, epIP := range epIPs {
			extIP := revExt[epIP]
			fmt.Fprintf(ipt, "-A %s -s %s -j SNAT --to-source %s %s\n",
				snatChain, epIP, extIP,
				iptCommentf("%s: external IP", revExtKey[epIP]))
		}

		fmt.Fprint(ipt, "COMMIT\n")

		newHash := xxhash.Checksum64(ipt.Bytes())
		if prevHash == newHash {
			continue
		}

		log.Print("ext-iptables: rules have changed, updating")
		rules := ipt.Bytes()

		// setup iptables command
		var cmd *exec.Cmd
		if *netns == "" {
			cmd = exec.Command("iptables-restore", "--noflush")
		} else {
			cmd = exec.Command("ip", "netns", "exec", *netns, "iptables-restore", "--noflush")
		}

		cmd.Stdin = ipt
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err := cmd.Run()
		if err != nil {
			log.Print("ext-iptables: failed to restore iptables rules: ", err, "\n", string(rules))
			continue
		}

		prevHash = newHash
	}
}

func iptComment(comment string) string {
	return fmt.Sprintf("-m comment --comment %q", comment)
}

func iptCommentf(pattern string, values ...interface{}) string {
	return iptComment(fmt.Sprintf(pattern, values...))
}

func iptRandom(idx, count int) string {
	proba := 1.0 / float64(count-idx)
	if proba == 1 {
		return ""
	}
	return fmt.Sprintf(" -m statistic --mode random --probability %.4f", proba)
}