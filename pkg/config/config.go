package config

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	home "github.com/mitchellh/go-homedir"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

//go:embed meshtk.yaml
var DefaultConfig string

type Config struct {
	Ctx            context.Context `json:"-"` //NOTE: Not being used yet.
	Version        string          `json:"__v"`
	HomeFolder     string
	Cwd            string
	VerboseLevel   string
	DateTime       string
	Stdin          []byte      `json:"-"`
	Stdout         io.Writer   `json:"-"`
	Log            *log.Logger `json:"-"`
	LogFolder      string
	ConfigFileName string `json:"-"`

	Release struct {
		Date    string
		Version string
		Hash    string
	} `json:"release"`

	Mqtt        Mqtt `json:"MQTT"`
	Meshtastic  Meshtastic
	NodeInfo    NodeInfo
	TextMessage TextMessage
	Fleet       []Fleet
	Server      Server

	NodeDbPath string `default:"./meshtk.db"`

	WasSuccess bool
}

type Server struct {
	InspectorListenAddress string `default:"localhost:50051"`
	ProxyListenAddress     string `default:"0.0.0.0:1883"`
	ProxyForwardAddress    string `default:"localhost:1884"`
	BlockFilenameTmpl      string `default:"blocklist.{{.DTS}}.log"`
	MaxMBLogSize           int    `default:"100"`
	UseS3Bucket            bool   `default:"true"`
	S3BucketRegion         string `default:"us-east-1"`
	S3BucketName           string `default:"mestk-blocklist-20250101"`
	S3BucketPrefix         string `default:"meshtk/blocklist"`
	CheckLogIntervalMins   int    `default:"15"`
	CheckLogIntervalSecs   int    `default:"10"`
	ShouldLogBlocks        bool   `default:"true"`
	ShouldLogAllows        bool   `default:"true"`
}

type Fleet struct {
	Id                     string     `json:"Id"`
	Description            string     `json:"Description"`
	Seed                   string     `json:"Seed"`
	NodesPerRampInterval   []int      `json:"NodesPerRampInterval"`
	NodesPerSteadyInterval []int      `json:"NodesPerSteadyInterval"`
	Distribution           string     `json:"Distribution" default:"uniform"`
	BroadcastGitterSec     int        `json:"BroadcastGitterSec"`
	LatLongAltGitter       int        `json:"LatLongAltGitter"`
	TextMessageGitterSec   int        `json:"TextMessageGitterSec"`
	NodeDbPath             string     `default:"./fleet.default.db"`
	BehaviourTag           []string   `default:"[ 'small', 'friendly' ]"`
	BehaviourSecs          int        `default:"60"`
	RampUpSecs             int        `default:"120"`
	RampSteadySecs         int        `default:"60"`
	RampDownSecs           int        `default:"120"`
	Movement               []Movement `json:"Movement"`
	ChatBot                []ChatBot  `json:"ChatBot"`
	ShortNameTmpl          string     `default:"M{fleetId}{nodeId}"`
	LongNameTmpl           string     `default:"mtk-{{.fleetId}}{{.nodeId}}-{{shortseed}}"`
	OtpUrl                 string     `default:""`
}

type Coordinate struct {
	Latitude  int32 `default:"361515048"`
	Longitude int32 `default:"-1151535588"`
	Altitude  int32 `default:"420"`
	Precision int   `default:"32"`
}

type Movement struct {
	Type      string       `default:"start"`
	Latitude  int32        `default:"361515048"`
	Longitude int32        `default:"-1151535588"`
	Altitude  int32        `default:"420"`
	Precision int32        `default:"32"`
	GPXFile   string       `default:"./gpx/route.gpx"`
	GPXCoords []Coordinate `default:"[]"`
	Travel    string       `default:"point-to-point"`
}

type ChatBot struct {
	Type            string   `json:"Type" default:"greeting"`
	Message         []string `json:"Message" default:"[]"`
	RequiresOTP     bool     `json:"RequiresOTP" default:"false"`
	UnlocksChatMode bool     `json:"UnlocksChatMode" default:"false"`
}

type Mqtt struct {
	BrokerUri string `default:"tcp://mqtt.meshtastic.org:1883"`
	Username  string `default:"meshdev"`
	Password  string `json:"-" default:"larg4cats"`
	ClientId  string `default:"meshtk-abcd1234"`
}

type Meshtastic struct {
	Channels []struct {
		Slot        string `default:"primary"`
		Name        string `default:"LongFast"`
		EncryptKey  string `json:"-" default:"AQ=="`
		IsEncrypted bool   `default:"true" `
		IsPrimary   bool   `default:"true"`
	}
}

type NodeInfo struct {
	ClientId             string   `default:"!1234ABCD"`
	ChannelSlot          string   `default:"primary"`
	ShortName            string   `default:"🍀"`
	HWModelId            uint32   `default:"43"`
	RoleId               uint32   `default:"0"`
	Topic                string   `default:"msh/US/2/e/LongFast"`
	MapTopic             string   `default:"msh/US/2/e/map/"`
	LongName             string   `default:"KPH MeshTK#1"`
	SubscribedTopics     []string `default:"['msh/US/LongFast']"`
	BroadcastToAll       bool     `default:"true"`
	BroadcastMessage     string   `default:"hello world"`
	BroadcastNodes       []string `default:"[]"`
	BroadcastOnLoad      bool     `default:"false"`
	BroadcastIntervalSec int      `default:"300"`
	PKI                  struct {
		PrivateKey string `default:""`
		PublicKey  string `default:""`
	}
	Latitude    float64 `default:"0"`
	Longitude   float64 `default:"0"`
	Altitude    float64 `default:"420"`
	Precision   float64 `default:"32"`
	Firmware    string  `default:"2.5.20.4c69420"`
	Region      string  `default:"US"`
	ModemPreset string  `default:"LONG_FAST"`
}

type TextMessage struct {
	Topic       string `default:"msh/US/2/e/LongFast"`
	ChannelSlot string `default:"primary"`
}

func NewConfig() (c *Config) {
	c = new(Config)

	c.Init()
	c.Read()

	return c
}

func (c *Config) Init() {
	c.Stdout = os.Stdout
	c.Ctx = context.TODO()
	c.VerboseLevel = "info"
	c.DateTime = time.Now().Format("20060102")

	homedir, err := home.Dir()
	if err != nil {
		c.Log.Fatal(fmt.Sprintf("failed to detect home directory: %v", err))
	}

	cwd, err := os.Getwd()
	if err != nil {
		c.Log.Fatal(err)
	}

	c.Cwd = cwd
	c.HomeFolder = homedir
}

func (c *Config) Read() {
	viper.SetConfigType("yaml")
	viper.SetEnvPrefix("meshtk")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	errDefault := viper.MergeConfig(strings.NewReader(DefaultConfig))
	if errDefault != nil {
		log.Errorf("default config wasn't valid: %s", errDefault)
		panic(errDefault)
	}
	viper.Unmarshal(&c)

	viper.AddConfigPath(c.HomeFolder)
	viper.AddConfigPath(c.Cwd)
	viper.SetConfigName("meshtk")

	errCwd := viper.MergeInConfig()
	if errCwd != nil {
		// Check for '-c' argument and use the following argument as the config file
		args := os.Args
		for i := range len(args) - 1 {
			if args[i] == "-c" {
				configFile := args[i+1]
				viper.SetConfigFile(configFile)
				err := viper.MergeInConfig()
				if err != nil {
					log.Warnf("failed to load config from file '%s': %s", configFile, err)
					return
				}
				break
			}
		}
	}

	viper.Unmarshal(&c)

	viper.AutomaticEnv()
	viper.Unmarshal(&c)

	//Slurp stdin into a variable
	fi, _ := os.Stdin.Stat() // get the FileInfo struct describing the standard input.
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		bytes, _ := io.ReadAll(os.Stdin)
		c.Stdin = bytes
	}

	c.WasSuccess = true
}
