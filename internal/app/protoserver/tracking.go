package protoserver

import (
	"net"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// ConnectionInfo stores information about a client connection
type ConnectionInfo struct {
	ClientID      string
	Username      string
	Password      string
	SocketAddress string
	ConnectTime   int64
}

func (*ProtoBufServerCmd) TrackConnection(conn net.Conn) (socketAddr string) {
	if conn.RemoteAddr() != nil {
		socketAddr = conn.RemoteAddr().String()
	}

	if proxyConn, ok := conn.(*proxyproto.Conn); ok {
		proxyHeader := proxyConn.ProxyHeader()
		if proxyHeader != nil && proxyHeader.SourceAddr != nil {
			socketAddr = proxyHeader.SourceAddr.String()
		}
	}
	return socketAddr
}

func (n *ProtoBufServerCmd) SetConnTrack(ip *InspectorPacket, socketAddress string) {
	n.ConnMutex.RLock()
	connInfo, exists := n.ConnTrack[socketAddress]
	n.ConnMutex.RUnlock()
	if exists {
		ip.Track = connInfo
	}
}

func (n *ProtoBufServerCmd) SetupTracker() {
	n.ConnTrack = make(map[string]*ConnectionInfo)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			i := 0
			now := time.Now().Unix()
			n.ConnMutex.Lock()
			for socketAddr, connInfo := range n.ConnTrack {
				if now-connInfo.ConnectTime > 180 {
					delete(n.ConnTrack, socketAddr)
					i++
				}
			}
			n.ConnMutex.Unlock()
			n.Config.Log.Tracef("Connection track cleanup completed: %d connections removed", i)
		}
	}()
}
