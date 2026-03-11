package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"text/template"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/admin"
	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/internal/credcache"
	"github.com/whereiskurt/meshtk/pkg/config"
	"github.com/whereiskurt/meshtk/pkg/network"
)

type ServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}

	ConnTrack map[string]*ConnectionInfo // maps connection ID to client ID
	ConnMutex sync.RWMutex

	Ciphers              []cipher.Block
	PacketDecider        Decider        // Interface for making packet routing decisions
	Authenticator        Authenticator  // Interface for MQTT CONNECT credential validation
	Limiters             []network.Limiter
	LogFileMutex         sync.RWMutex
	InspectorLogger      *log.Logger
	InspectorLogFilename string

	// Concrete types for admin API wiring (lowercase = unexported)
	cache         *credcache.Cache
	store         *credcache.DynamoDBStore
	authenticator *credcache.CacheAuthenticator
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

	// Initialize credential cache and authenticator for proxy mode
	cache, err := credcache.NewCache(
		c.Server.CredCache.TTLSecs,
		c.Server.CredCache.MaxSizeMB,
	)
	if err != nil {
		c.Log.Fatalf("Failed to create credential cache: %v", err)
	}

	store, err := credcache.NewDynamoDBStore(
		c.Server.CredCache.TableName,
		c.Server.CredCache.TableRegion,
		c.Server.CredCache.DynamoDBEndpoint,
	)
	if err != nil {
		c.Log.Fatalf("Failed to create DynamoDB store: %v", err)
	}

	n.cache = cache
	n.store = store
	n.authenticator = credcache.NewCacheAuthenticator(cache, store,
		credcache.WithNegativeTTL(time.Duration(c.Server.CredCache.NegativeTTLSecs)*time.Second),
	)
	n.Authenticator = n.authenticator

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

	// Launch admin HTTP server if configured
	adminAddr := n.Config.Server.AdminListenAddress
	if adminAddr != "" {
		adminSrv := admin.NewServer(n.cache, n.store, n.authenticator, nil)
		go func() {
			n.Config.Log.Infof("Admin API listening on %s", adminAddr)
			if err := http.ListenAndServe(adminAddr, adminSrv.Handler()); err != nil {
				n.Config.Log.Errorf("Admin server error: %v", err)
			}
		}()
	}

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
		
		// Test S3 connectivity on startup if S3 is enabled
		if n.Config.Server.UseS3Bucket {
			n.TestS3Connectivity()
		}
		
		ticker := time.NewTicker((time.Duration(n.Config.Server.CheckLogIntervalMins) * time.Minute) + (time.Duration(n.Config.Server.CheckLogIntervalSecs) * time.Second))
		defer ticker.Stop()

		for range ticker.C {
			filename := filepath.Join(n.Config.Cwd, n.Config.LogFolder, n.InspectorLogFilename)
			fileSizeMB := 0.0
			fileInfo, err := os.Stat(filename)
			if err != nil {
				n.Config.Log.Errorf("failed to get file info for %s: %v", filename, err)
				return
			}
			fileSizeMB = float64(fileInfo.Size()) / (1024 * 1024)

			if fileSizeMB >= float64(n.Config.Server.MaxMBLogSize) {
				n.Config.Log.Infof("Log rotation triggered: %s is %.2f MB (limit: %d MB). Uploading to S3...", n.InspectorLogFilename, fileSizeMB, n.Config.Server.MaxMBLogSize)
				n.MoveToBucket(filename)
				//This rolls the log file over by creating a new one
				n.SetupInspectorLogger()
				n.Config.Log.Infof("Log rotation complete. New log file created.")
			} else {
				n.Config.Log.Tracef("Log file %s size: %.2f MB (limit: %d MB)", n.InspectorLogFilename, fileSizeMB, n.Config.Server.MaxMBLogSize)
			}
		}
	}()
}

func (n *ServerCmd) TestS3Connectivity() {
	n.Config.Log.Info("Testing S3 connectivity on startup...")
	
	s3BucketRegion := n.Config.Server.S3BucketRegion
	s3BucketName := n.Config.Server.S3BucketName
	s3BucketPrefix := n.Config.Server.S3BucketPrefix
	
	awsRegion := os.Getenv("AWS_REGION")
	if awsRegion == "" {
		awsRegion = "us-east-1"
	}
	
	scopy, err := network.NewS3Mover(
		awsRegion,
		s3BucketRegion,
		s3BucketName,
	)
	if err != nil {
		n.Config.Log.Errorf("S3 startup test failed - could not create S3 mover: %v", err)
		return
	}
	
	// Write startup test file
	err = scopy.WriteStartupTest(s3BucketPrefix)
	if err != nil {
		n.Config.Log.Errorf("S3 startup test failed: %v", err)
		n.Config.Log.Error("WARNING: S3 uploads may not work. Please check AWS credentials and bucket permissions.")
	} else {
		n.Config.Log.Info("S3 startup test passed - connectivity verified")
	}
}

func (n *ServerCmd) MoveToBucket(filename string) {

	if !n.Config.Server.UseS3Bucket {
		return
	}
	s3BucketRegion := n.Config.Server.S3BucketRegion
	s3BucketName := n.Config.Server.S3BucketName
	s3BucketPrefix := n.Config.Server.S3BucketPrefix

	awsRegion := os.Getenv("AWS_REGION")
	if awsRegion == "" {
		awsRegion = "us-east-1"
	}

	n.Config.Log.Debugf("Initializing S3Mover: awsRegion=%s, bucketRegion=%s, bucketName=%s", awsRegion, s3BucketRegion, s3BucketName)
	
	scopy, err := network.NewS3Mover(
		awsRegion,
		s3BucketRegion,
		s3BucketName,
	)
	if err != nil {
		n.Config.Log.Errorf("failed to create S3 mover: %v", err)
		return
	}

	n.Config.Log.Debugf("Moving file %s to S3 with prefix %s", filename, s3BucketPrefix)
	res, err2 := scopy.Move(filename, s3BucketPrefix)
	if err2 != nil {
		n.Config.Log.Errorf("failed to move log file to S3: %v", res.ErrorMessage)
	} else {
		n.Config.Log.Infof("Successfully uploaded file to S3: %s", res.URL)
	}
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

	mw := io.MultiWriter(f)
	if n.Config.VerboseLevel == "debug" || n.Config.VerboseLevel == "trace" {
		mw = io.MultiWriter(os.Stdout, f)
	}

	logger.SetOutput(mw)
	n.InspectorLogFilename = filename

	n.InspectorLogger = logger

}
