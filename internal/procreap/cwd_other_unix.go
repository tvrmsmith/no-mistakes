//go:build unix && !linux

package procreap

// processCWDs resolves each pid's working directory. Outside Linux there is no
// /proc to read, so batched lsof calls answer for the candidate set.
func processCWDs(pids []int) (map[int]string, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	return lsofCWDs(pids)
}
