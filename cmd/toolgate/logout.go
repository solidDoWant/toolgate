package main

import (
	"fmt"
	"io"

	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

// logout removes the stored refresh token from the keychain and the fallback
// file.
func logout(stdout, stderr io.Writer) int {
	store, err := tokenstore.Default()
	if err == nil {
		err = store.Delete()
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate logout: %v\n", err)

		return 1
	}

	_, _ = fmt.Fprintln(stdout, "Signed out.")

	return 0
}
