// Command keyset writes a new AES-256-GCM Tink keyset as cleartext JSON to
// the file named by its only argument. The e2e harness gives it to the
// gateway as the secrets keyset.
package main

import (
	"fmt"
	"os"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: keyset FILE")
		os.Exit(2)
	}

	if err := write(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "keyset:", err)
		os.Exit(1)
	}
}

func write(path string) error {
	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	if err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // harness path
	if err != nil {
		return err
	}

	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(file)); err != nil {
		_ = file.Close()

		return err
	}

	return file.Close()
}
