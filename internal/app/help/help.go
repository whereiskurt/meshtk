package help

import (
	"bytes"
	_ "embed"
	"regexp"
	"strings"
	"text/template"

	"github.com/whereiskurt/meshtk/pkg/config"
)

var RegexSafeName = regexp.MustCompile(`[^a-zA-Z0-9\-]+`)

// Embed the templates
var (
	//go:embed global.tmpl
	GlobalTmpl string
	//go:embed nodeinfo.tmpl
	NodeInfoTmpl string
)

var TEMPLATES = strings.Join([]string{
	GlobalTmpl,
	NodeInfoTmpl,
}, "\n")

var Templates = template.Must(template.New("render").Parse(TEMPLATES))

func GlobalHelp(c *config.Config) string {
	return Render("GlobalHelp", c)
}

func NodeInfoHelp(c *config.Config) string {
	return Render("NodeInfoHelp", c)
}

func Render(name string, c *config.Config) string {
	name = RegexSafeName.ReplaceAllString(name, "")

	var output bytes.Buffer

	Templates.Parse(`{{ template "` + name + `" . }}`)

	err := Templates.Execute(&output, c)
	if err != nil {
		c.Log.Fatalf("name '%s' is an invalid template name: %s", name, err)
	}

	return output.String()
}
