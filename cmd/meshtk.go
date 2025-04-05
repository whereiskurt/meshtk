package main

import (
	"os"

	"github.com/whereiskurt/meshtk/internal/app"
	"github.com/whereiskurt/meshtk/pkg/config"
)

func main() {
	app := app.NewApp(config.NewConfig())
	app.Run().Destroy()
	os.Exit(0)
}
