package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"text/template"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
	"github.com/whereiskurt/meshtk/pkg/network"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

type ServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}

	ConnTrack map[string]*ConnectionInfo // maps connection ID to client ID
	ConnMutex sync.RWMutex

	Ciphers              []cipher.Block
	PacketDecider        Decider // Interface for making packet routing decisions
	Limiters             []network.Limiter
	LogFileMutex         sync.RWMutex
	InspectorLogger      *log.Logger
	InspectorLogFilename string
}

func NewAESCipher(key []byte) cipher.Block {
	c, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	return c
}

func NewServer(c *config.Config) (n *ServerCmd) {
	n = new(ServerCmd)
	n.Config = c
	n.SetupTracker()
	n.LoadCiphers(c)
	n.LoadInspectorRules()
	return n
}

func (n *ServerCmd) LoadCiphers(c *config.Config) {
	for _, channel := range n.Config.Meshtastic.Channels {
		base64Key := channel.EncryptKey
		keyBytes, err := base64.StdEncoding.DecodeString(base64Key)
		if err != nil {
			c.Log.Fatalf("The %s channel key '%s' is invalid hex: %+v", channel.Name, base64Key, err)
		}
		// Expand the single byte key to 16 bytes for AES-256
		if len(keyBytes) == 1 && base64Key == "AQ==" {
			keyBytes = append(keyBytes, make([]byte, 15)...)
		}
		n.Ciphers = append(n.Ciphers, NewAESCipher(keyBytes))
	}
}

func (n *ServerCmd) DecryptMeshtastic(id, from uint32, payload []byte) (decoded *meshtastic.Data, hexkey string, c *cipher.Block, err error) {
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], id)
	binary.LittleEndian.PutUint32(nonce[8:], from)
	decrypted := make([]byte, len(payload))

	for k, cipherInstance := range n.Ciphers {
		hexKey := n.Config.Meshtastic.Channels[k].EncryptKey

		cipher.NewCTR(cipherInstance, nonce).XORKeyStream(decrypted, payload)
		decoded = new(meshtastic.Data)
		if err := proto.Unmarshal(decrypted, decoded); err == nil {
			return decoded, hexKey, &cipherInstance, nil
		}

	}
	return nil, "", nil, fmt.Errorf("failed to decrypt data with any cipher")
}

func (n *ServerCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.ServerHelp(n.Config))
}

func (n *ServerCmd) ProtobufServer(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("protobuf.InspectorServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.StartProtobufServer()
}

func (n *ServerCmd) ProxyServer(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("protobuf.ProxyServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.InitInspectorLogger()
	n.StartProxyServer()
}

func (n *ServerCmd) StartProtobufServer() error {
	address := n.Config.Server.InspectorListenAddress

	ln, err := net.Listen("tcp", address)
	if err != nil {
		n.Config.Log.Errorf("Failed to listen: %v", err)
		return err
	}
	defer ln.Close()

	go func() {
		n.Config.Log.Infof("Meshtastic protobuff inspector server listening on %s", address)
		for {
			conn, err := ln.Accept()
			if err == nil {
				go n.handleProtobuf(conn)
			}
		}
	}()

	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the inspector server gracefully...")
	return nil

}

func (n *ServerCmd) StartProxyServer() error {
	address := n.Config.Server.ProxyListenAddress
	backendAddress := n.Config.Server.ProxyForwardAddress

	listener, err := net.Listen("tcp", address)
	if err != nil {
		n.Config.Log.Fatal("listen error:", err)
	}
	proxyListener := &proxyproto.Listener{Listener: listener}
	defer func() {
		proxyListener.Close()
	}()

	n.Config.Log.Infof("🚀 Proxy server started and listening on %v with Proxy Protocol support", address)
	n.Config.Log.Infof("Forwarding connections to backend at %v", backendAddress)

	n.ConnMutex = sync.RWMutex{}
	n.ConnTrack = make(map[string]*ConnectionInfo)

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil || conn.RemoteAddr() == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			go func(c net.Conn) {
				n.handleProxy(c)
			}(conn)
		}
	}()

	n.Config.Log.Infof("Proxy server is ready for connections")
	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the proxy server gracefully...")
	return nil
}

func (n *ServerCmd) InitInspectorLogger() {
	go func() {
		n.SetupInspectorLogger()
		ticker := time.NewTicker(time.Duration(n.Config.Server.CheckLogIntervalMins) * time.Minute)
		defer ticker.Stop()

		for range ticker.C {

			fileSizeMB := 0.0
			fileInfo, err := os.Stat(filepath.Join(n.Config.Cwd, n.Config.LogFolder, n.InspectorLogFilename))
			if err != nil {
				n.Config.Log.Errorf("failed to get file info for %s: %v", n.InspectorLogFilename, err)
				return
			}
			fileSizeMB = float64(fileInfo.Size()) / (1024 * 1024)

			if fileSizeMB > float64(n.Config.Server.MaxMBLogSize) {
				n.SetupInspectorLogger()
			} else {
				n.Config.Log.Tracef("logfile %s size: %.2f MB smaller than %.2f", n.InspectorLogFilename, fileSizeMB, float64(n.Config.Server.MaxMBLogSize))
			}
		}
	}()
}

type SimpleFormatter struct {
	TimestampFormat string
}

func (f *SimpleFormatter) Format(entry *log.Entry) ([]byte, error) {
	timestamp := entry.Time.Format(f.TimestampFormat)
	return []byte(fmt.Sprintf("%s %s\n", timestamp, entry.Message)), nil
}

func (n *ServerCmd) SetupInspectorLogger() {
	logger := log.New()

	logger.Level = n.Config.Log.Level

	logger.SetFormatter(&SimpleFormatter{
		TimestampFormat: "2006-01-02 15:04:05.000",
	})

	path := filepath.Join(n.Config.Cwd, n.Config.LogFolder)
	filename := n.Config.Server.BlockFilenameTmpl

	tmplData := map[string]interface{}{
		"DTS": time.Now().Format("20060102.150405"),
	}
	tmpl, _ := template.New("log.filename").Parse(filename)

	var tmplBuffer bytes.Buffer
	if err := tmpl.Execute(&tmplBuffer, tmplData); err != nil {
		panic(fmt.Sprintf("failed to execute render logfile name %s: %v", filename, err))
	}
	filename = tmplBuffer.String()

	abs, _ := filepath.Abs(path)
	os.MkdirAll(abs, 0777)

	f, err := os.OpenFile(filepath.Join(abs, filename), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		panic(fmt.Sprintf("error: cannot open file: %v", err))
	}

	logger.SetOutput(f)
	n.InspectorLogFilename = filename

	n.InspectorLogger = logger

}
