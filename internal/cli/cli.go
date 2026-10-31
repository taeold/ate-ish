// Package cli is ate-ish's kubectl-ate-style client: create/get/suspend/
// delete verbs against a running node's admin API, over --endpoint (or
// ATE_URL as an env fallback).
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"

	"github.com/taeold/ate-ish/internal/records"
)

const usage = `usage: ate-ish VERB KIND [NAME] [flags]
  server: --endpoint (default http://localhost:8080), or $ATE_URL
  auth: gcloud identity token, if gcloud is available and logged in

  ate-ish create atespace NAME
  ate-ish create actor-template -f FILE
  ate-ish create actor NAME --template T [--tag TAG] -a ATESPACE
  ate-ish create tag NAME --actor ACTOR -a ATESPACE
  ate-ish create egress-policy ACTOR -f FILE -a ATESPACE
  ate-ish suspend actor NAME -a ATESPACE
  ate-ish delete actor NAME -a ATESPACE
  ate-ish get actors|tags -a ATESPACE
  ate-ish get workers`

type actorRow struct {
	Name, Template, Status, Owner string
	Generation                    int64
	Created                       time.Time
	EgressRules                   bool
}

// Run parses args as one kubectl-ate verb and runs it.
func Run(args []string) error {
	fs := pflag.NewFlagSet("ate-ish", pflag.ContinueOnError)
	atespace := fs.StringP("atespace", "a", "", "atespace")
	file := fs.StringP("file", "f", "", "YAML file")
	template := fs.String("template", "", "actor template")
	tag := fs.String("tag", "", "tag to start the actor from")
	actor := fs.String("actor", "", "actor to tag")
	endpoint := fs.String("endpoint", "", "ingress URL (default http://localhost:8080, or $ATE_URL)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	words := append(fs.Args(), "", "", "")
	verb, kind, name := words[0], words[1], words[2]
	as := "/admin/atespaces/" + *atespace
	base := resolveEndpoint(*endpoint)

	var out []byte
	var err error
	switch verb + " " + kind {
	case "create atespace":
		out, err = call(base, "PUT", "/admin/atespaces/"+name, nil)
	case "create actor-template":
		var body []byte
		if body, err = yamlFile(*file); err == nil {
			var t records.Template
			json.Unmarshal(body, &t)
			out, err = call(base, "PUT", "/admin/atespaces/"+t.Metadata.Atespace+"/templates/"+t.Metadata.Name, body)
		}
	case "create actor":
		out, err = call(base, "PUT", as+"/actors/"+name, map[string]string{"template": *template, "tag": *tag})
	case "create tag":
		out, err = call(base, "PUT", as+"/tags/"+name, map[string]string{"actor": *actor})
	case "create egress-policy":
		var body []byte
		if body, err = yamlFile(*file); err == nil {
			out, err = call(base, "PUT", as+"/actors/"+name+"/egress-policy", body)
		}
	case "suspend actor":
		out, err = call(base, "POST", as+"/actors/"+name+"/suspend", nil)
	case "delete actor":
		out, err = call(base, "DELETE", as+"/actors/"+name, nil)
	case "get actors":
		out, err = call(base, "GET", as+"/actors", nil)
	case "get tags":
		out, err = call(base, "GET", as+"/tags", nil)
	case "get workers":
		out, err = call(base, "GET", "/admin/workers", nil)
	default:
		return fmt.Errorf("unknown command %q\n%s", strings.TrimSpace(verb+" "+kind), usage)
	}
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	defer tw.Flush()
	switch kind {
	case "actor", "actors":
		if verb == "delete" {
			fmt.Fprintf(tw, "deleted actor %s\n", name)
			return nil
		}
		var rows []actorRow
		if kind == "actor" {
			rows = make([]actorRow, 1)
			err = json.Unmarshal(out, &rows[0])
		} else {
			err = json.Unmarshal(out, &rows)
		}
		fmt.Fprintln(tw, "ATESPACE\tNAME\tTEMPLATE\tSTATE\tWORKER\tGENERATION\tEGRESS POLICY\tAGE")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%v\t%s\n", *atespace, r.Name, r.Template, r.Status,
				or(r.Owner, "<none>"), r.Generation, r.EgressRules, time.Since(r.Created).Round(time.Second))
		}
	case "workers":
		var leases map[string]records.Lease
		err = json.Unmarshal(out, &leases)
		fmt.Fprintln(tw, "ID\tADDR\tACTORS\tLEASE\tHOSTED")
		for id, l := range leases {
			state := "live"
			if !l.Live() {
				state = "expired"
			}
			sort.Strings(l.Hosted)
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\n", id, l.Addr, len(l.Hosted), l.Capacity, state, strings.Join(l.Hosted, ","))
		}
	case "tags":
		var tags []string
		err = json.Unmarshal(out, &tags)
		fmt.Fprintln(tw, "ATESPACE\tNAME")
		for _, t := range tags {
			fmt.Fprintf(tw, "%s\t%s\n", *atespace, t)
		}
	default:
		fmt.Fprintf(tw, "%s %s: %s", verb, kind, out)
	}
	return err
}

func yamlFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return yaml.YAMLToJSON(data)
}

// resolveEndpoint is --endpoint, or $ATE_URL, or http://localhost:8080.
func resolveEndpoint(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if url := os.Getenv("ATE_URL"); url != "" {
		return url
	}
	return "http://localhost:8080"
}

// call sends one admin request to base, with a gcloud identity token when
// gcloud is available and logged in (Cloud Run's own IAM needs it). A
// local node checks no such token itself, so a laptop with no gcloud at
// all (or one just not logged in) still drives it fine.
func call(base, method, path string, body any) ([]byte, error) {
	data, ok := body.([]byte)
	if !ok && body != nil {
		data, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, strings.TrimSuffix(base, "/")+path, bytes.NewReader(data))
	if token, err := exec.Command("gcloud", "auth", "print-identity-token").Output(); err == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(out))
	}
	return out, nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
