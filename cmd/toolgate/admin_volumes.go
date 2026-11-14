package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"github.com/solidDoWant/toolgate/internal/volumesapi"
)

const adminVolumesUsage = `Usage: toolgate admin volumes <command>

Commands:
  list [--owner PRINCIPAL] [--json]   List the persistent volumes
  delete ID...                        Mark volumes for deletion
`

// maxVolumePages bounds how many pages `list` follows.
const maxVolumePages = 50

// maxVolumesBody bounds one page of the volume list: 200 rows fit.
const maxVolumesBody = 4 << 20

// adminVolumes runs `toolgate admin volumes`. The commands call the gateway
// as the signed-in user; they need no gateway configuration file.
func adminVolumes(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, adminVolumesUsage)

		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch args[0] {
	case "list":
		return volumesList(ctx, args[1:], stdout, stderr)
	case "delete":
		return volumesDelete(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, adminVolumesUsage)

		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "toolgate admin volumes: unknown command %q\n\n%s", args[0], adminVolumesUsage)

		return 2
	}
}

func volumesList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("admin volumes list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	owner := flags.String("owner", "", "only the per-user volumes of this principal")
	asJSON := flags.Bool("json", false, "print the volumes as JSON")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate admin volumes list: unexpected argument %q\n", flags.Arg(0))

		return 2
	}

	err := withAccess(ctx, func(access gatewayAccess) error {
		return listVolumesWith(ctx, access, *owner, *asJSON, stdout, stderr)
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate admin volumes list: %v\n", err)

		return 1
	}

	return 0
}

// listVolumesWith reads every page, up to maxVolumePages, and prints them.
func listVolumesWith(ctx context.Context, access gatewayAccess, owner string, asJSON bool, stdout, stderr io.Writer) error {
	var volumes []volumesapi.Volume

	after, more := "", false

	for page := 0; page < maxVolumePages; page++ {
		query := url.Values{}
		if owner != "" {
			query.Set("owner", owner)
		}

		if after != "" {
			query.Set("after", after)
		}

		path := volumesapi.Path
		if len(query) > 0 {
			path += "?" + query.Encode()
		}

		list, err := fetchVolumePage(ctx, access, path)
		if err != nil {
			return err
		}

		volumes = append(volumes, list.Volumes...)
		after, more = list.Next, list.Next != ""

		if !more {
			break
		}
	}

	if more {
		_, _ = fmt.Fprintf(stderr, "toolgate admin volumes list: stopped after %d pages; narrow the list with --owner\n", maxVolumePages)
	}

	if asJSON {
		if volumes == nil {
			volumes = []volumesapi.Volume{}
		}

		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")

		return encoder.Encode(volumesapi.List{Volumes: volumes})
	}

	return printVolumes(stdout, volumes)
}

func fetchVolumePage(ctx context.Context, access gatewayAccess, path string) (volumesapi.List, error) {
	response, err := access.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return volumesapi.List{}, err
	}

	defer func() { _ = response.Body.Close() }()

	var list volumesapi.List
	if err := json.NewDecoder(io.LimitReader(response.Body, maxVolumesBody)).Decode(&list); err != nil {
		return volumesapi.List{}, fmt.Errorf("read the gateway's answer: %w", err)
	}

	return list, nil
}

func printVolumes(stdout io.Writer, volumes []volumesapi.Volume) error {
	if len(volumes) == 0 {
		_, _ = fmt.Fprintln(stdout, "There are no volumes.")

		return nil
	}

	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "ID\tSERVER\tVOLUME\tSCOPE\tOWNER\tSIZE\tIN USE BY\tLAST USED\tDELETION")

	for _, volume := range volumes {
		inUse := "-"
		if volume.InUseBy != nil {
			inUse = *volume.InUseBy
		}

		deletion := "-"
		if volume.DeleteReason != "" {
			deletion = volume.DeleteReason
		}

		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			volume.ID, volume.Server, volume.Volume, volume.Scope, dashIfEmpty(volume.Owner), formatSize(volume.SizeBytes),
			inUse, volume.LastUsedAt.UTC().Format(time.RFC3339), deletion)
	}

	if err := table.Flush(); err != nil {
		return errors.Join(errors.New("write the list"), err)
	}

	return nil
}

func dashIfEmpty(text string) string {
	if text == "" {
		return "-"
	}

	return text
}

// formatSize writes bytes in the largest binary unit that keeps a whole or
// one-decimal number.
func formatSize(size int64) string {
	units := []string{"B", "Ki", "Mi", "Gi", "Ti"}
	value, unit := float64(size), 0

	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}

	if value == float64(int64(value)) {
		return fmt.Sprintf("%d%s", int64(value), units[unit])
	}

	return fmt.Sprintf("%.1f%s", value, units[unit])
}

func volumesDelete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, "toolgate admin volumes delete: give at least one volume ID\n")

		return 2
	}

	missing := false

	err := withAccess(ctx, func(access gatewayAccess) error {
		var err error

		missing, err = deleteVolumesWith(ctx, access, args, stdout)

		return err
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate admin volumes delete: %v\n", err)

		return 1
	}

	if missing {
		return 1
	}

	return 0
}

// deleteVolumesWith marks each volume and prints the result. It goes on past
// an unknown ID and reports whether there was one. Any other error stops it.
func deleteVolumesWith(ctx context.Context, access gatewayAccess, ids []string, stdout io.Writer) (bool, error) {
	missing := false

	for _, id := range ids {
		response, err := access.do(ctx, http.MethodDelete, volumesapi.Path+"/"+url.PathEscape(id), nil)

		var failure *gatewayError
		if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
			missing = true

			_, _ = fmt.Fprintf(stdout, "%s: no such volume\n", id)

			continue
		}

		if err != nil {
			return missing, fmt.Errorf("%s: %w", id, err)
		}

		var volume volumesapi.Volume

		err = json.NewDecoder(io.LimitReader(response.Body, maxVolumesBody)).Decode(&volume)
		_ = response.Body.Close()

		if err != nil {
			return missing, fmt.Errorf("%s: read the gateway's answer: %w", id, err)
		}

		if volume.InUseBy != nil {
			_, _ = fmt.Fprintf(stdout, "%s: marked for deletion; instance %s uses it, so it is deleted after that instance stops\n", id, *volume.InUseBy)
		} else {
			_, _ = fmt.Fprintf(stdout, "%s: marked for deletion\n", id)
		}
	}

	return missing, nil
}
