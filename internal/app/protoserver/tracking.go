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

func (*ProtoBufServerCmd) TrackConnection(conn net.Conn) (clientAddr string) {
	if conn.RemoteAddr() != nil {
		clientAddr = conn.RemoteAddr().String()
	}

	if proxyConn, ok := conn.(*proxyproto.Conn); ok {
		proxyHeader := proxyConn.ProxyHeader()
		if proxyHeader != nil && proxyHeader.SourceAddr != nil {
			clientAddr = proxyHeader.SourceAddr.String()
		}
	}
	return clientAddr
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
