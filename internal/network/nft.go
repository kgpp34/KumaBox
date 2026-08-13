package network

import "strings"

const (
	nftTable = "kumabox"
	nftChain = "postrouting"
)

func nftNATRuleHandles(output []byte, cidr string) []string {
	var handles []string
	for line := range strings.Lines(string(output)) {
		fields := strings.Fields(line)
		if !containsFieldSequence(fields, []string{"ip", "saddr", cidr, "masquerade"}) {
			continue
		}
		for index := len(fields) - 2; index >= 0; index-- {
			if fields[index] == "handle" {
				handles = append(handles, strings.TrimSuffix(fields[index+1], ";"))
				break
			}
		}
	}
	return handles
}

func containsFieldSequence(fields, sequence []string) bool {
	if len(sequence) == 0 || len(fields) < len(sequence) {
		return false
	}
	for start := 0; start <= len(fields)-len(sequence); start++ {
		matched := true
		for index := range sequence {
			if fields[start+index] != sequence[index] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
