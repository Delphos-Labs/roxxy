// inspect-candidates: read npm candidates JSONL from the oracle, fetch
// each tarball from registry.npmjs.org, run inspect.NPMInspector, and
// emit one finding-per-line JSONL plus a per-package summary.
//
// Not shipped — used to validate inspector at scale and surface
// install-hook-bearing packages among the anomaly candidates.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/inspect"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

type candidate struct {
	Detector string `json:"detector"`
	Format   string `json:"format"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	File     string `json:"file"`
}

type report struct {
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	URL      string            `json:"url"`
	HTTPCode int               `json:"http_code,omitempty"`
	Bytes    int               `json:"bytes,omitempty"`
	Error    string            `json:"error,omitempty"`
	Findings []inspect.Finding `json:"findings,omitempty"`
}

func main() {
	in := flag.String("in", "", "candidates jsonl (npm only)")
	out := flag.String("out", "", "output jsonl for reports")
	tier := flag.String("tier", "composite", "detector tier to inspect (composite|size|cadence|all)")
	parallel := flag.Int("parallel", 8, "parallel HTTP fetches")
	maxN := flag.Int("max", 0, "max candidates to inspect (0 = all)")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: inspect-candidates -in <npm.jsonl> -out <report.jsonl> [-tier composite] [-max N]")
		os.Exit(2)
	}

	f, err := os.Open(*in)
	if err != nil {
		fatal(err)
	}
	defer util.DeferClose(f)

	var cands []candidate
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var c candidate
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			continue
		}
		if c.Format != "npm" {
			continue
		}
		if *tier != "all" && c.Detector != *tier {
			continue
		}
		cands = append(cands, c)
		if *maxN > 0 && len(cands) >= *maxN {
			break
		}
	}
	if err := sc.Err(); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "loaded %d candidates (tier=%s)\n", len(cands), *tier)

	outF, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer util.DeferClose(outF)
	w := bufio.NewWriter(outF)
	defer func() { _ = w.Flush() }()
	var writeMu sync.Mutex

	sem := make(chan struct{}, *parallel)
	var wg sync.WaitGroup
	var done, blocked, warned, errored int64

	client := &http.Client{Timeout: 60 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i, c := range cands {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, c candidate) {
			defer wg.Done()
			defer func() { <-sem }()
			r := inspectOne(ctx, client, c)
			writeMu.Lock()
			b, _ := json.Marshal(r)
			_, _ = w.Write(b)
			_ = w.WriteByte('\n')
			writeMu.Unlock()
			d := atomic.AddInt64(&done, 1)
			if r.Error != "" {
				atomic.AddInt64(&errored, 1)
			} else {
				var b, wn bool
				for _, fnd := range r.Findings {
					if fnd.Severity == inspect.SeverityBlock {
						b = true
					}
					if fnd.Severity == inspect.SeverityWarn {
						wn = true
					}
				}
				if b {
					atomic.AddInt64(&blocked, 1)
				}
				if wn {
					atomic.AddInt64(&warned, 1)
				}
			}
			if d%25 == 0 {
				fmt.Fprintf(os.Stderr, "  progress: %d/%d  block=%d warn=%d err=%d\n",
					d, len(cands), atomic.LoadInt64(&blocked),
					atomic.LoadInt64(&warned), atomic.LoadInt64(&errored))
			}
		}(i, c)
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "done: total=%d block=%d warn=%d err=%d\n",
		done, blocked, warned, errored)
}

func inspectOne(ctx context.Context, client *http.Client, c candidate) report {
	url := tarballURL(c)
	r := report{Name: c.Name, Version: c.Version, URL: url}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer util.DeferClose(resp.Body)
	r.HTTPCode = resp.StatusCode
	if resp.StatusCode != 200 {
		r.Error = "http " + resp.Status
		return r
	}
	// Cap read at 32 MiB; bigger artifacts get inspected truncated by inspector itself.
	lr := io.LimitReader(resp.Body, 32*1024*1024)
	body, err := io.ReadAll(lr)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Bytes = len(body)
	findings, err := inspect.NPMInspector{}.Inspect(body, c.Name, c.Version)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			r.Error = err.Error()
		}
		return r
	}
	r.Findings = findings
	return r
}

func tarballURL(c candidate) string {
	// registry.npmjs.org tarball convention: /<name>/-/<basename>.tgz
	return "https://registry.npmjs.org/" + c.Name + "/-/" + c.File
}

func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
