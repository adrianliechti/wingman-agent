package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
)

// One row per scenario, model, and directory, so two builds kept in separate directories line up.
func writeSummary(w io.Writer, directories []string) error {
	type key struct{ scenario, model, agent, label string }
	groups := map[key][]benchResult{}
	for _, directory := range directories {
		directory = strings.TrimSpace(directory)
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		label := filepath.Base(filepath.Clean(directory))
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".otel.json") {
				continue
			}
			contents, err := os.ReadFile(filepath.Join(directory, name))
			if err != nil {
				return err
			}
			var result benchResult
			if json.Unmarshal(contents, &result) != nil || result.Agent == "" || result.Scenario == "" {
				continue
			}
			k := key{result.Scenario, result.Model, result.Agent, label}
			groups[k] = append(groups[k], result)
		}
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		return strings.Join([]string{a.scenario, a.model, a.agent, a.label}, "\x00") < strings.Join([]string{b.scenario, b.model, b.agent, b.label}, "\x00")
	})

	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "SCENARIO\tMODEL\tAGENT\tRUNS\tPASS\tSECONDS\tREQUESTS\tLEAD\tINPUT TOK\tCACHED\tOUTPUT TOK\tTOOL ERR\tSIGNALS")
	for _, k := range keys {
		runs := groups[k]
		passed := 0
		var seconds, requests, lead, input, output, toolErrors []float64
		var inputTotal, cachedTotal int64
		signals := map[string]int{}
		for _, run := range runs {
			if run.TestsPassed && run.Error == "" {
				passed++
			}
			seconds = append(seconds, float64(run.TotalMS)/1000)
			observed := run.Behavior
			if observed == nil {
				continue
			}
			requests = append(requests, float64(observed.ModelRequests))
			lead = append(lead, float64(observed.LeadRequests))
			input = append(input, float64(observed.InputTokens))
			output = append(output, float64(observed.OutputTokens))
			failures := 0
			for _, count := range observed.ToolErrors {
				failures += count
			}
			toolErrors = append(toolErrors, float64(failures))
			inputTotal += observed.InputTokens
			cachedTotal += observed.CachedReadTokens
			for name, count := range observed.Signals {
				signals[name] += count
			}
			// How the lead waited is the point of the delegation scenarios.
			for _, name := range []string{"agent_wait", "agent_output"} {
				if count := observed.ToolCalls[name]; count > 0 {
					signals[name] += count
				}
			}
		}
		cached := "-"
		if inputTotal > 0 {
			cached = fmt.Sprintf("%.0f%%", 100*float64(cachedTotal)/float64(inputTotal))
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d/%d\t%.1f\t%.1f\t%.1f\t%.0f\t%s\t%.0f\t%.1f\t%s\n",
			k.scenario, k.model, k.agent+"@"+k.label, len(runs), passed, len(runs),
			median(seconds), median(requests), median(lead), median(input), cached, median(output), median(toolErrors), formatCounts(signals))
	}
	return table.Flush()
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(values))
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func formatCounts(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return strings.Join(parts, " ")
}
