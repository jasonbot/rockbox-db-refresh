package cmd

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
)

var App = &cli.App{
	Name:     "rbdb",
	Usage:    "Rockbox tagcache database builder and audio converter",
	Version:  "0.1.0",
	Commands: []*cli.Command{dbCommand, fixCommand, syncCommand, stockCommand},
}

func init() {
	cli.AppHelpTemplate = `{{.Name}} {{.Version}} — {{.Usage}}

USAGE:
   {{.Name}} <command> [options]

COMMANDS:
{{range .Commands}}
   {{.Name}}{{"\t"}}{{.Usage}}{{end}}
{{- if .UseShortOptionHandling}}
   Use "{{.Name}} <command> --help" for more information about a command.
{{- end}}
`
}

func Execute() {
	if err := App.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
