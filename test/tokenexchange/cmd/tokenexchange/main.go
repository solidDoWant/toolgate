// Command tokenexchange serves the test token endpoint over HTTPS.
//
//	-listen   listen address (default :8443)
//	-cert     PEM certificate file
//	-key      PEM key file
//	-lifetime how long each token is valid (default 5m)
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/solidDoWant/toolgate/test/tokenexchange"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "tokenexchange:", err)

		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("tokenexchange", flag.ContinueOnError)
	listen := flags.String("listen", ":8443", "listen address")
	cert := flags.String("cert", "", "PEM certificate file")
	key := flags.String("key", "", "PEM key file")
	lifetime := flags.Duration("lifetime", 5*time.Minute, "how long each token is valid")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if *cert == "" || *key == "" {
		return errors.New("-cert and -key are required: a token endpoint is served over HTTPS")
	}

	server := &http.Server{Addr: *listen, Handler: tokenexchange.New(*lifetime), ReadHeaderTimeout: 10 * time.Second}

	return server.ListenAndServeTLS(*cert, *key)
}
