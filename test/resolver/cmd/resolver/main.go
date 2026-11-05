// Command resolver serves the test resolver over HTTPS.
//
//	-listen   listen address (default :8443)
//	-cert     PEM certificate file
//	-key      PEM key file
//	-lifetime how long each value is valid (default 5m)
//
// With -write-cert DIR and -host HOST it instead writes a self-signed
// certificate for HOST to DIR/cert.pem and DIR/key.pem and exits.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/solidDoWant/toolgate/test/resolver"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "resolver:", err)

		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("resolver", flag.ContinueOnError)
	listen := flags.String("listen", ":8443", "listen address")
	cert := flags.String("cert", "", "PEM certificate file")
	key := flags.String("key", "", "PEM key file")
	lifetime := flags.Duration("lifetime", 5*time.Minute, "how long each value is valid")
	writeCert := flags.String("write-cert", "", "write a self-signed certificate to this directory and exit")
	host := flags.String("host", "", "the IP address or DNS name of the certificate -write-cert writes")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if *writeCert != "" {
		if *host == "" {
			return errors.New("-write-cert needs -host")
		}

		return resolver.WriteCertificate(*writeCert, *host)
	}

	if *cert == "" || *key == "" {
		return errors.New("-cert and -key are required: a resolver is served over HTTPS")
	}

	server := &http.Server{Addr: *listen, Handler: resolver.New(*lifetime), ReadHeaderTimeout: 10 * time.Second}

	return server.ListenAndServeTLS(*cert, *key)
}
