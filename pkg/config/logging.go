package config

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

func (c *Config) SetupLogging() {
	c.Log = log.New()
	path := filepath.Join(c.Cwd, c.LogFolder)
	clientFilename := c.DateTime + ".client.log"
	setupLogging(c.Log, path, clientFilename, c.VerboseLevel)
}

func setupLogging(l *log.Logger, path, filename, verbose string) {

	l.SetFormatter(&log.TextFormatter{})

	switch strings.ToLower(verbose) {
	case "error":
		l.Level = log.ErrorLevel
	case "warn":
		l.Level = log.WarnLevel
	case "info":
		l.Level = log.InfoLevel
	case "debug":
		l.Level = log.DebugLevel
	case "trace":
		l.Level = log.TraceLevel
	}

	abs, _ := filepath.Abs(path)
	os.MkdirAll(abs, 0777)

	f, err := os.OpenFile(filepath.Join(abs, filename), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		fmt.Printf("error: cannot open file: %v", err)
	}

	mw := io.MultiWriter(f)
	if l.IsLevelEnabled(log.TraceLevel) || l.IsLevelEnabled(log.DebugLevel) {
		mw = io.MultiWriter(os.Stdout, f)
	}
	l.SetOutput(mw)
}
