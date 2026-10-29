package main

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
)

const usage = `usage: ate-ish VERB KIND [NAME] [flags]     (server: $ATE_URL, auth: gcloud identity token)
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

func runCLI(args []string) error {
	fs := pflag.NewFlagSet("ate-ish", pflag.ContinueOnError)
	atespace := fs.StringP("atespace", "a", "", "atespace")
	file := fs.StringP("file", "f", "", "YAML file")
	template := fs.String("template", "", "actor template")
	tag := fs.String("tag", "", "tag to start the actor from")
	actor := fs.String("actor", "", "actor to tag")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	words := append(fs.Args(), "", "", "")
	verb, kind, name := words[0], words[1], words[2]
	as := "/admin/atespaces/" + *atespace

	var out []byte
	var err error
	switch verb + " " + kind {
	case "create atespace":
		out, err = call("PUT", "/admin/atespaces/"+name, nil)
	case "create actor-template":
		var body []byte
		if body, err = yamlFile(*file); err == nil {
			var t Template
			json.Unmarshal(body, &t)
			out, err = call("PUT", "/admin/atespaces/"+t.Metadata.Atespace+"/templates/"+t.Metadata.Name, body)
		}
	case "create actor":
		out, err = call("PUT", as+"/actors/"+name, map[string]string{"template": *template, "tag": *tag})
	case "create tag":
		out, err = call("PUT", as+"/tags/"+name, map[string]string{"actor": *actor})
	case "create egress-policy":
		var body []byte
		if body, err = yamlFile(*file); err == nil {
			out, err = call("PUT", as+"/actors/"+name+"/egress-policy", body)
		}
	case "suspend actor":
		out, err = call("POST", as+"/actors/"+name+"/suspend", nil)
	case "delete actor":
		out, err = call("DELETE", as+"/actors/"+name, nil)
	case "get actors":
		out, err = call("GET", as+"/actors", nil)
	case "get tags":
		out, err = call("GET", as+"/tags", nil)
	case "get workers":
		out, err = call("GET", "/admin/workers", nil)
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
		var leases map[string]Lease
		err = json.Unmarshal(out, &leases)
		fmt.Fprintln(tw, "ID\tADDR\tACTORS\tLEASE\tHOSTED")
		for id, l := range leases {
			state := "live"
			if !l.live() {
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

// call sends one admin request to $ATE_URL with a gcloud identity token.
func call(method, path string, body any) ([]byte, error) {
	base := os.Getenv("ATE_URL")
	if base == "" {
		return nil, fmt.Errorf("set ATE_URL to the ingress URL")
	}
	data, ok := body.([]byte)
	if !ok && body != nil {
		data, _ = json.Marshal(body)
	}
	token, err := exec.Command("gcloud", "auth", "print-identity-token").Output()
	if err != nil {
		return nil, fmt.Errorf("gcloud auth print-identity-token: %w", err)
	}
	req, _ := http.NewRequest(method, strings.TrimSuffix(base, "/")+path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
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
