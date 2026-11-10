package ingest

import (
	"encoding/json"
	"net/http"
	"strconv"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
)

const (
	// StdoutOutput as the logs endpoint writes each received log record to
	// standard output as one JSON line instead of forwarding it.
	StdoutOutput = "stdout"

	// logLineSource marks the lines so queries can tell them from the
	// gateway's own logs.
	logLineSource = "claude-code"

	resultWritten = "written"
	resultFailed  = "failed"
)

// logLineResourceKeys are the only resource attributes written to a line:
// those that identify the client.
var logLineResourceKeys = []string{"service.name", "service.version", "os.type", "host.arch"}

// logLine is one OTLP log record as written to stdout.
type logLine struct {
	Source       string         `json:"source"`
	Event        string         `json:"event,omitempty"`
	Time         string         `json:"time_unix_nano"`
	ObservedTime string         `json:"observed_time_unix_nano,omitempty"`
	Severity     string         `json:"severity,omitempty"`
	Body         string         `json:"body,omitempty"`
	Attributes   map[string]any `json:"attributes"`
	Resource     map[string]any `json:"resource"`
}

// writeLine writes one whole line. The mutex keeps lines of concurrent
// requests apart; each line is one Write call so it also stays whole next to
// the gateway's own output.
func (i *Ingest) writeLine(line []byte) error {
	i.outputMutex.Lock()
	defer i.outputMutex.Unlock()

	_, err := i.output.Write(line)

	return err
}

// logLines builds one line per log record.
func logLines(logs plog.Logs) []logLine {
	var lines []logLine

	for _, resource := range logs.ResourceLogs().All() {
		identity := map[string]any{}

		for _, key := range logLineResourceKeys {
			if value, found := resource.Resource().Attributes().Get(key); found {
				identity[key] = value.AsRaw()
			}
		}

		for _, scope := range resource.ScopeLogs().All() {
			for _, record := range scope.LogRecords().All() {
				lines = append(lines, newLogLine(record, identity))
			}
		}
	}

	return lines
}

func newLogLine(record plog.LogRecord, resource map[string]any) logLine {
	line := logLine{
		Source:     logLineSource,
		Event:      eventName(record),
		Time:       strconv.FormatUint(uint64(record.Timestamp()), 10),
		Attributes: record.Attributes().AsRaw(),
		Resource:   resource,
	}

	if record.ObservedTimestamp() != 0 {
		line.ObservedTime = strconv.FormatUint(uint64(record.ObservedTimestamp()), 10)
	}

	switch {
	case record.SeverityText() != "":
		line.Severity = record.SeverityText()
	case record.SeverityNumber() != plog.SeverityNumberUnspecified:
		line.Severity = record.SeverityNumber().String()
	}

	if record.Body().Type() == pcommon.ValueTypeStr {
		line.Body = record.Body().Str()
	}

	return line
}

// eventName reads the event name from the record's event name field, else its
// `event.name` attribute, else its string body.
func eventName(record plog.LogRecord) string {
	if name := record.EventName(); name != "" {
		return name
	}

	if value, found := record.Attributes().Get("event.name"); found && value.Type() == pcommon.ValueTypeStr {
		return value.Str()
	}

	if record.Body().Type() == pcommon.ValueTypeStr {
		return record.Body().Str()
	}

	return ""
}

// writeLogs writes the stamped records to the output, one line each, and
// answers the client. A record that can't be written counts as failed and
// makes the request fail so the client retries.
func (i *Ingest) writeLogs(writer http.ResponseWriter, signal signal, export plogotlp.ExportRequest, jsonAnswer bool) {
	failed := false

	for _, line := range logLines(export.Logs()) {
		encoded, err := json.Marshal(line)
		if err == nil {
			err = i.writeLine(append(encoded, '\n'))
		}

		if err != nil {
			failed = true

			i.logRecords.WithLabelValues(resultFailed).Inc()

			continue
		}

		i.logRecords.WithLabelValues(resultWritten).Inc()
	}

	if failed {
		i.logger.Warn("writing OTLP log records to stdout failed")
		unavailable(writer, "")

		return
	}

	i.answer(writer, nil, signal, nil, jsonAnswer)
}
