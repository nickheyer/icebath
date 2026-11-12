package cron

import (
	"context"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Absolute path of the urgency classifier script shipped beside the cron package of the install
func classifyItemsScriptPath() string {
	return filepath.Join(pm.RepoRoot(), "cron", "scripts", "classify_items.py")
}

// Curated starter automation offered as a suggestion with its stable dedup key and create_job spec
type CatalogEntry struct {
	Key, Title, Description string
	JobSpec                 [][2]string
}

// Ordered job spec of an entry as a fresh object
func (e CatalogEntry) jobSpecObject() *pyjson.Object {
	obj := pyjson.NewObject()
	for _, kv := range e.JobSpec {
		obj.Set(kv[0], kv[1])
	}
	return obj
}

// Built in automations offered out of the box, none of them scheduled until accepted
var Catalog = []CatalogEntry{
	{
		Key:   "catalog:daily-briefing",
		Title: "Daily briefing",
		Description: "Every morning at 8am, a short briefing: today's calendar, " +
			"weather, and anything urgent waiting on you.",
		JobSpec: [][2]string{
			{"prompt", "Produce a concise morning briefing for the user: today's " +
				"calendar events, the local weather, and any urgent items " +
				"(unread important email, due tasks). Keep it short and " +
				"scannable. If you have no connected data sources, give a brief " +
				"general good-morning with the date and offer to connect " +
				"calendar/email."},
			{"schedule", "0 8 * * *"},
			{"name", "Daily briefing"},
			{"deliver", "origin"},
		},
	},
	{
		Key:   "catalog:important-mail-monitor",
		Title: "Important-mail monitor",
		Description: "Check your inbox periodically and ping you ONLY about mail " +
			"that actually needs attention — never the newsletters.",
		JobSpec: [][2]string{
			{"prompt", "Check the user's inbox for new messages since the last run. " +
				"For each candidate, judge urgency against this rule: surface " +
				"only mail that needs a reply today, is from a manager/family " +
				"member, or mentions a deadline. Pipe candidates through the " +
				"urgency classifier (run `python3 -m cron.scripts.classify_items " +
				"--threshold 7 --criteria ...` from the icebath-agent install — " +
				"resolve the script path at run time, do not assume a fixed " +
				"location) and deliver ONLY what it returns. If nothing " +
				"clears the bar, respond with [SILENT] so the user is not " +
				"pinged. Requires a connected mail source; if none is " +
				"configured, explain how to connect one and then stop."},
			{"schedule", "every 30m"},
			{"name", "Important-mail monitor"},
			{"deliver", "origin"},
		},
	},
	{
		Key:   "catalog:weekly-review",
		Title: "Weekly review",
		Description: "Every Sunday evening, a recap of the week: what got done, " +
			"what's still open, and what's coming up next week.",
		JobSpec: [][2]string{
			{"prompt", "Produce a weekly review for the user: summarize what was " +
				"accomplished this week, list still-open items, and preview " +
				"next week's calendar. Pull from whatever sources are connected " +
				"(calendar, task tools, recent conversations). Keep it tight."},
			{"schedule", "0 18 * * 0"},
			{"name", "Weekly review"},
			{"deliver", "origin"},
		},
	},
	{
		Key:   "catalog:standup-reminder",
		Title: "Workday start reminder",
		Description: "A weekday nudge at 9am with your day's agenda and top " +
			"priorities, so you start focused.",
		JobSpec: [][2]string{
			{"prompt", "Give the user a brief weekday start-of-day nudge: their " +
				"calendar for today and the 1-3 highest-priority things to " +
				"focus on, inferred from recent context and any task tools. " +
				"Encouraging, short, one message."},
			{"schedule", "0 9 * * 1-5"},
			{"name", "Workday start reminder"},
			{"deliver", "origin"},
		},
	},
}

// Suggestion registrar the catalog seeds through, the store's add_suggestion shape
type SuggestionAdder func(title, description, source string, jobSpec any, dedupKey string) (*pyjson.Object, error)

// Registers catalog entries as pending suggestions, keys restricting the set, returning the records created
func SeedCatalogSuggestions(ctx context.Context, add SuggestionAdder, keys []string) ([]*pyjson.Object, error) {
	if add == nil {
		add = openCronStore(home.Dir(ctx), time.Now).AddSuggestion
	}
	var wanted map[string]bool
	if len(keys) > 0 {
		wanted = make(map[string]bool, len(keys))
		for _, k := range keys {
			wanted[k] = true
		}
	}
	created := []*pyjson.Object{}
	for _, entry := range Catalog {
		if wanted != nil && !wanted[entry.Key] {
			continue
		}
		rec, err := add(entry.Title, entry.Description, "catalog", entry.jobSpecObject(), entry.Key)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			created = append(created, rec)
		}
	}
	return created, nil
}
