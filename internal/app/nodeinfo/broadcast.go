package nodeinfo

import "github.com/whereiskurt/meshtk/pkg/config"

// broadcastText returns the channel text DoBroadcast should publish alongside
// the NodeInfo/Position beacon, and whether to publish one at all.
//
// An empty BroadcastMessage -- the default -- means the beacon announces the
// node and its position but stays silent in chat. Setting it opts the node into
// broadcasting that text to 0xffffffff on every rebroadcast tick.
func broadcastText(c *config.Config) ([]byte, bool) {
	if c.NodeInfo.BroadcastMessage == "" {
		return nil, false
	}
	return []byte(c.NodeInfo.BroadcastMessage), true
}
