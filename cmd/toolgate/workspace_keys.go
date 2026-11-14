package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"text/tabwriter"
	"time"

	"github.com/solidDoWant/toolgate/internal/client/atomicfile"
	"github.com/solidDoWant/toolgate/internal/client/attestation"
)

const (
	workspaceKeysPath = "/api/v1/workspace-keys"
)

var keyNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)

// registeredKey is the gateway's answer to a registration that already
// finished (200) or is waiting for approval (202).
type registeredKey struct {
	Status      string  `json:"status"`
	Request     string  `json:"request"`
	URL         string  `json:"url"`
	Name        string  `json:"name"`
	Workspace   string  `json:"workspace"`
	Fingerprint string  `json:"fingerprint"`
	Replaces    *string `json:"replaces"`
}

type listedKey struct {
	Name         string     `json:"name"`
	Workspace    string     `json:"workspace"`
	Fingerprint  string     `json:"fingerprint"`
	Status       string     `json:"status"`
	RegisteredAt time.Time  `json:"registeredAt"`
	LastUsedAt   *time.Time `json:"lastUsedAt"`
}

type listedKeys struct {
	Enabled bool        `json:"enabled"`
	Keys    []listedKey `json:"keys"`
}

type revokedKey struct {
	Revoked        bool `json:"revoked"`
	DeniedRequests int  `json:"deniedRequests"`
}

// runWorkspaceKeys handles `workspace register` and `workspace keys`. It
// returns the exit code.
func runWorkspaceKeys(ctx context.Context, env workspaceEnv, command string, args []string, withAccess func(func(gatewayAccess) error) error) int {
	label := "toolgate workspace " + command

	var run func(access gatewayAccess) error

	switch command {
	case "register":
		name, keyFile, ok := parseRegisterArgs(args, env.stderr)
		if !ok {
			return 2
		}

		path, err := keyFilePath(env, name, keyFile)
		if err != nil {
			_, _ = fmt.Fprintf(env.stderr, "%s: %s\n", label, err)

			return 1
		}

		run = func(access gatewayAccess) error { return registerKey(ctx, env, access, name, path) }
	default:
		sub, rest := "", args

		if len(args) > 0 {
			sub, rest = args[0], args[1:]
		}

		label += " " + sub

		switch sub {
		case "list":
			if len(rest) > 0 {
				_, _ = fmt.Fprintf(env.stderr, "%s: unexpected argument %q\n", label, rest[0])

				return 2
			}

			run = func(access gatewayAccess) error { return listKeys(ctx, env, access) }
		case "revoke":
			if len(rest) != 1 {
				_, _ = fmt.Fprintf(env.stderr, "%s: give the name of one key\n", label)

				return 2
			}

			run = func(access gatewayAccess) error { return revokeKey(ctx, env, access, rest[0]) }
		default:
			_, _ = fmt.Fprintf(env.stderr, "toolgate workspace keys: use list or revoke NAME\n\n%s", workspaceUsage)

			return 2
		}
	}

	if err := withAccess(run); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "%s: %s\n", label, errorText(err))

		return 1
	}

	return 0
}

func parseRegisterArgs(args []string, stderr io.Writer) (string, string, bool) {
	flags := flag.NewFlagSet("toolgate workspace register", flag.ContinueOnError)
	flags.SetOutput(stderr)

	keyFile := flags.String("key-file", "", "`PATH` of the private key (default: workspace-keys/NAME.pem in the configuration directory)")

	var positional []string

	for {
		if err := flags.Parse(args); err != nil {
			return "", "", false
		}

		if flags.NArg() == 0 {
			break
		}

		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}

	if len(positional) != 1 {
		_, _ = fmt.Fprintln(stderr, "toolgate workspace register: give the name of one key")

		return "", "", false
	}

	if !keyNamePattern.MatchString(positional[0]) {
		_, _ = fmt.Fprintln(stderr, "toolgate workspace register: a key name has 1 to 63 letters, digits, `.`, `_` or `-`.")

		return "", "", false
	}

	return positional[0], *keyFile, true
}

func keyFilePath(env workspaceEnv, name, keyFile string) (string, error) {
	if keyFile == "" {
		dir, err := env.configDirectory()
		if err != nil {
			return "", err
		}

		keyFile = filepath.Join(dir, "workspace-keys", name+".pem")
	}

	absolute, err := filepath.Abs(keyFile)
	if err != nil {
		return "", fmt.Errorf("resolve the key file path: %w", err)
	}

	return absolute, nil
}

func (e workspaceEnv) configDirectory() (string, error) {
	if e.configDir != nil {
		return e.configDir()
	}

	return configDir()
}

// loadOrCreateKey loads the key at path, or makes and saves a new one.
func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	_, err := os.Stat(path)
	if err == nil {
		return attestation.LoadKeyFile(path)
	}

	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("make a key: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("encode the key: %w", err)
	}

	if err := atomicfile.Write(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}

	return private, nil
}

func registerKey(ctx context.Context, env workspaceEnv, access gatewayAccess, name, path string) error {
	private, err := loadOrCreateKey(path)
	if err != nil {
		return err
	}

	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("the key is not an Ed25519 key")
	}

	body, err := json.Marshal(map[string]string{"name": name, "publicKey": base64.RawURLEncoding.EncodeToString(public)})
	if err != nil {
		return err
	}

	response, err := access.do(ctx, http.MethodPost, workspaceKeysPath, bytes.NewReader(body))
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	var result registeredKey
	if err := json.NewDecoder(io.LimitReader(response.Body, maxWorkspaceBody)).Decode(&result); err != nil {
		return fmt.Errorf("read the gateway's answer: %w", err)
	}

	if response.StatusCode == http.StatusOK {
		_, _ = fmt.Fprintf(env.stdout, "Key `%s` is already registered and active. Its workspace is `%s`.\n",
			terminalSafe(name), terminalSafe(result.Workspace))

		return nil
	}

	_, _ = fmt.Fprintf(env.stdout, "Registered key `%s` with fingerprint %s.\n", terminalSafe(name), terminalSafe(result.Fingerprint))

	if result.Replaces != nil {
		_, _ = fmt.Fprintf(env.stdout, "It replaces the key with fingerprint %s.\n", terminalSafe(*result.Replaces))
	}

	_, _ = fmt.Fprintf(env.stdout, "Approve it at %s. Make sure that the page shows the same fingerprint.\n"+
		"Waiting… (Ctrl-C stops waiting; the request stays open until it expires.)\n", terminalSafe(result.URL))

	if err := awaitDecision(ctx, env, access, result.Request); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(env.stdout, "Key `%s` is active. Sessions that use it run in workspace `%s`. "+
		"To use it, run: toolgate configure --attestation-key-file %s\n", terminalSafe(name), terminalSafe(result.Workspace), terminalSafe(path))

	return nil
}

func listKeys(ctx context.Context, env workspaceEnv, access gatewayAccess) error {
	response, err := access.do(ctx, http.MethodGet, workspaceKeysPath, nil)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	var list listedKeys
	if err := json.NewDecoder(io.LimitReader(response.Body, maxWorkspaceBody)).Decode(&list); err != nil {
		return fmt.Errorf("read the gateway's answer: %w", err)
	}

	if !list.Enabled {
		_, _ = fmt.Fprintln(env.stdout, "Workspace keys are off on this gateway. These keys don't attest.")
	}

	table := tabwriter.NewWriter(env.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tWORKSPACE\tFINGERPRINT\tSTATUS\tREGISTERED\tLAST USED")

	for _, key := range list.Keys {
		lastUsed := "never"
		if key.LastUsedAt != nil {
			lastUsed = key.LastUsedAt.UTC().Format(time.RFC3339)
		}

		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(key.Name), terminalSafe(key.Workspace),
			terminalSafe(key.Fingerprint), terminalSafe(key.Status), key.RegisteredAt.UTC().Format(time.RFC3339), lastUsed)
	}

	if err := table.Flush(); err != nil {
		return errors.Join(errors.New("write the list"), err)
	}

	return nil
}

func revokeKey(ctx context.Context, env workspaceEnv, access gatewayAccess, name string) error {
	response, err := access.do(ctx, http.MethodDelete, workspaceKeysPath+"/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	var result revokedKey
	if err := json.NewDecoder(io.LimitReader(response.Body, maxWorkspaceBody)).Decode(&result); err != nil {
		return fmt.Errorf("read the gateway's answer: %w", err)
	}

	_, _ = fmt.Fprintf(env.stdout, "Revoked key `%s`.\n", terminalSafe(name))

	if result.DeniedRequests > 0 {
		_, _ = fmt.Fprintf(env.stdout, "Denied %d pending requests for `%s`.\n", result.DeniedRequests, terminalSafe(name))
	}

	return nil
}
