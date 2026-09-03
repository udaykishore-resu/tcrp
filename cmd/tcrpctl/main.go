// Command tcrpctl talks to a running platform over its HTTP API.
//
// It exists so that the operational paths — asking a question, seeing what is
// waiting for a human, granting or refusing an approval, checking the
// conformance gate — can be exercised the way an operator would, through the
// same API the dashboard uses, rather than through a second code path that
// might behave differently.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "tcrpctl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tcrpctl — a client for a running platform

  tcrpctl ask "<question>"          start a governed run
  tcrpctl runs                      list runs
  tcrpctl run <id>                  show one run
  tcrpctl approvals                 list what is waiting for a human
  tcrpctl grant <approval-id>       approve, as --as
  tcrpctl deny <approval-id>        refuse, as --as
  tcrpctl exposure [counterparty]   exposure, one name or all
  tcrpctl breaks                    open reconciliation breaks
  tcrpctl supply                    cross-network supply reconciliation
  tcrpctl audit                     the audit chain and whether it verifies
  tcrpctl conformance               the invariants and the gate
  tcrpctl version                   build information

Flags: --url (default http://localhost:8080), --as <principal>, --note <text>, --json
`)
}

type client struct {
	url  string
	as   string
	note string
	raw  bool
	http *http.Client
}

func run(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	url := fs.String("url", envOr("TCRP_URL", "http://localhost:8080"), "platform base URL")
	as := fs.String("as", "operator", "acting principal")
	note := fs.String("note", "", "note recorded with an approval decision")
	raw := fs.Bool("json", false, "print the raw JSON response")
	// Go's flag package stops at the first non-flag argument, which would make
	// `tcrpctl exposure CP-DELTA --url ...` silently ignore the flag and talk to
	// the wrong host. Separating the two up front is a small price for a client
	// that behaves the way every other command-line tool does.
	flags, rest := splitArgs(args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	c := &client{
		url: strings.TrimRight(*url, "/"), as: *as, note: *note, raw: *raw,
		http: &http.Client{Timeout: 30 * time.Second},
	}
	switch cmd {
	case "version":
		fmt.Printf("tcrpctl %s (commit %s, built %s)\n", version, commit, builtAt)
		return nil
	case "ask":
		q := strings.TrimSpace(strings.Join(rest, " "))
		if q == "" {
			return errors.New(`give me a question, e.g. tcrpctl ask "what is our exposure to CP-DELTA?"`)
		}
		return c.post(ctx, "/v1/runs", map[string]any{"query": q})
	case "runs":
		return c.get(ctx, "/v1/runs")
	case "run":
		if len(rest) != 1 {
			return errors.New("usage: tcrpctl run <id>")
		}
		return c.get(ctx, "/v1/runs/"+rest[0])
	case "approvals":
		return c.get(ctx, "/v1/approvals")
	case "grant", "deny":
		if len(rest) != 1 {
			return fmt.Errorf("usage: tcrpctl %s <approval-id> --as risk-manager", cmd)
		}
		return c.post(ctx, "/v1/approvals/"+rest[0]+"/"+cmd, map[string]any{"note": c.note})
	case "exposure":
		if len(rest) == 1 {
			return c.get(ctx, "/v1/exposure/"+strings.ToUpper(rest[0]))
		}
		return c.get(ctx, "/v1/exposure")
	case "breaks":
		return c.get(ctx, "/v1/breaks")
	case "supply":
		return c.get(ctx, "/v1/supply")
	case "audit":
		return c.get(ctx, "/v1/audit/verify")
	case "conformance":
		return c.get(ctx, "/v1/conformance")
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func (c *client) get(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *client) post(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPost, path, body)
}

func (c *client) do(ctx context.Context, method, path string, body any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The development principal header. A production listener refuses it, and
	// a real deployment presents a signed assertion minted by the institution's
	// identity provider instead.
	req.Header.Set("X-TCRP-As", c.as)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w (is the platform running? try: tcrpd serve)", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		raw = pretty.Bytes()
	}
	if resp.StatusCode >= 400 {
		fmt.Fprintln(os.Stderr, string(raw))
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	fmt.Println(string(raw))
	return nil
}

// splitArgs separates flags (and their values) from positional arguments.
// A flag is a token beginning with "-"; it consumes the next token as its value
// unless it was written as --name=value or is one of the known booleans.
func splitArgs(args []string) (flags, positional []string) {
	booleans := map[string]bool{"--json": true, "-json": true, "-h": true, "--help": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") || booleans[a] {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
