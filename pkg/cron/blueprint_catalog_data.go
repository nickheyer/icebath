package cron

// Curated in-repo catalog of automation blueprints
var blueprintCatalog = []AutomationBlueprint{
	{
		Key: "morning-brief", Title: "Morning briefing", Category: "daily",
		Description:      "A short daily briefing: today's calendar, weather, and anything urgent waiting on you.",
		ScheduleTemplate: "{minute} {hour} * * *",
		PromptTemplate:   "Produce a concise morning briefing for the user: today's calendar events, the local weather, and any urgent items. When Gmail/Google Calendar are connected, follow the google-workspace skill's references/daily-brief.md procedure (exact day window, conflict detection, meeting prep, mail-to-meeting links). Keep it short and scannable. If no data sources are connected, give a brief good-morning with the date and offer to connect calendar/email.",
		Slots:            []BlueprintSlot{blueprintTime("08:00"), blueprintDeliver},
		DeliverDefault:   "origin", Skills: []string{"google-workspace"}, Tags: []string{"daily", "briefing"},
	},
	{
		Key: "important-mail", Title: "Important-mail monitor", Category: "email",
		Description:      "Check your inbox periodically and ping you ONLY about mail that actually needs attention.",
		ScheduleTemplate: "*/{interval_min} * * * *",
		PromptTemplate:   "Check the user's inbox for new messages since the last run. Surface ONLY mail matching: {criteria}. Score candidates with the urgency classifier and deliver only what clears the bar; if nothing does, respond with [SILENT]. Requires a connected mail source; if none is configured, explain how to connect one and stop.",
		Slots:            []BlueprintSlot{{Name: "interval_min", Type: "enum", Label: "How often?", Default: "30", Options: []any{"15", "30", "60"}, Help: "minutes between checks", Strict: true}, {Name: "criteria", Type: "text", Label: "Only notify me if the mail…", Default: "needs a reply today, is from my manager or family, or mentions a deadline", Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: []string{"email-inbox-triage"}, Tags: []string{"email", "monitor"},
	},
	{
		Key: "weekly-review", Title: "Weekly review", Category: "weekly",
		Description:      "A weekly recap: what got done, what's still open, and what's coming up.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Run the weekly-review-planning skill's procedure for the user: review the completed week and coming 1-2 weeks across connected calendar, tasks, notes, and email; surface commitments, stalled projects, and waiting items; build a capacity-aware plan for next week. Recommendations and drafts only — no mutations without approval. Keep the output in the skill's seven-section shape.",
		Slots:            []BlueprintSlot{blueprintTime("18:00"), {Name: "day", Type: "enum", Label: "Which day?", Default: "sunday", Options: []any{"sunday", "monday", "friday", "saturday"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: []string{"weekly-review-planning"}, Tags: []string{"weekly", "review"},
	},
	{
		Key: "workday-start", Title: "Workday start reminder", Category: "daily",
		Description:      "A weekday nudge with your agenda and top priorities.",
		ScheduleTemplate: "{minute} {hour} * * 1-5",
		PromptTemplate:   "Give the user a brief weekday start-of-day nudge: today's calendar and the 1-3 highest-priority things to focus on, inferred from recent context and any task tools. Encouraging, short, one message.",
		Slots:            []BlueprintSlot{blueprintTime("09:00"), blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"daily", "focus"},
	},
	{
		Key: "custom-reminder", Title: "Custom reminder", Category: "general",
		Description:      "A recurring reminder in your own words, on your schedule.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Remind the user: {what}",
		Slots:            []BlueprintSlot{{Name: "what", Type: "text", Label: "Remind me to…", Default: "take a break and stretch", Strict: true}, blueprintTime("14:00"), {Name: "recurrence", Type: "weekdays", Label: "Repeat on", Default: "everyday", Options: []any{"everyday", "weekdays", "weekends"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"reminder"},
	},
	{
		Key: "evening-winddown", Title: "Evening wind-down", Category: "daily",
		Description:      "An end-of-day check-in: tomorrow's calendar at a glance and anything you should prep tonight.",
		ScheduleTemplate: "{minute} {hour} * * *",
		PromptTemplate:   "Give the user a short evening wind-down: tomorrow's calendar, any early commitments to prep for, and one gentle nudge to wrap up loose ends from today. Keep it calm and brief — one message. If no calendar is connected, just offer a friendly sign-off and the weather for tomorrow.",
		Slots:            []BlueprintSlot{blueprintTime("21:00"), blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"daily", "evening"},
	},
	{
		Key: "news-digest", Title: "Topic news digest", Category: "general",
		Description:      "A recurring digest on a topic you care about — deduped against what was already sent, so only genuinely new items land.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Search the web for new and noteworthy items about: {topic}. Dedupe against what you sent in previous runs — only include genuinely new developments. Deliver a tight digest of at most {count} bullets, each one line with a link. If nothing new since last run, respond with [SILENT].",
		Slots:            []BlueprintSlot{{Name: "topic", Type: "text", Label: "What topic?", Default: "AI and technology", Help: "a subject, product, person, or search phrase", Strict: true}, blueprintTime("18:00"), {Name: "recurrence", Type: "weekdays", Label: "Repeat on", Default: "weekdays", Options: []any{"everyday", "weekdays", "weekends"}, Strict: true}, {Name: "count", Type: "enum", Label: "How many bullets?", Default: "5", Options: []any{"3", "5", "8"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"digest", "research"},
	},
	{
		Key: "bill-renewal-watch", Title: "Bills & renewals reminder", Category: "general",
		Description:      "A heads-up before a recurring payment, subscription renewal, or due date — so nothing auto-charges by surprise.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Remind the user about an upcoming payment or renewal: {what}. Phrase it as an actionable heads-up (e.g. 'review or cancel before it renews'), not just a notification. One short message.",
		Slots:            []BlueprintSlot{{Name: "what", Type: "text", Label: "What's due?", Default: "my streaming subscription renews soon", Strict: true}, blueprintTime("10:00"), {Name: "recurrence", Type: "weekdays", Label: "Repeat on", Default: "everyday", Options: []any{"everyday", "weekdays", "weekends"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"reminder", "finance"},
	},
	{
		Key: "price-watch", Title: "Price & availability watch", Category: "general",
		Description:      "Watch an exact product, flight, hotel, or listing and alert when your price or availability condition is met.",
		ScheduleTemplate: "0 */{interval_h} * * *",
		PromptTemplate:   "Load the product-price-monitor skill and run the tick for this watch: {item}. Alert condition: {condition}. Compare the normalized all-in price/availability against stored state, suppress duplicate alerts, and never overwrite last-known-good state with a failed fetch. If no condition is met, respond with [SILENT]. On the first run, execute the skill's setup phase first: pin the exact item, verify one live fetch, and write the watch contract state file.",
		Slots:            []BlueprintSlot{{Name: "item", Type: "text", Label: "What exactly to watch?", Default: "a product URL or exact flight/hotel/listing description", Help: "URL or precise description — variant, dates, seller", Strict: true}, {Name: "condition", Type: "text", Label: "Alert me when…", Default: "the all-in price drops below my target", Help: "threshold price (state the currency), availability, or terms change", Strict: true}, {Name: "interval_h", Type: "enum", Label: "How often?", Default: "6", Options: []any{"1", "3", "6", "12", "24"}, Help: "hours between checks — be gentle with rate limits", Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: []string{"product-price-monitor"}, Tags: []string{"prices", "shopping", "travel", "monitor"},
	},
	{
		Key: "competitor-watch", Title: "Competitor news watch", Category: "general",
		Description:      "Track named companies for material news — launches, pricing, funding, filings — with a cited digest.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Load the competitor-news-monitor skill and run the tick for this watch: companies {companies}; event categories {categories}. Collect incrementally from the last cutoff, deduplicate by underlying event, score materiality against the watch contract, and deliver a cited digest of material events only. If there are no material events, respond with [SILENT]. On the first run, execute the skill's setup phase first: freeze the watchlist, build source coverage, and write the watch contract state file.",
		Slots:            []BlueprintSlot{{Name: "companies", Type: "text", Label: "Which companies?", Default: "two or three competitors, by canonical name", Help: "canonical names and domains; aliases help dedup", Strict: true}, {Name: "categories", Type: "text", Label: "Which events matter?", Default: "product launches, pricing changes, funding, partnerships, executive moves, incidents", Strict: true}, blueprintTime("09:00"), {Name: "recurrence", Type: "weekdays", Label: "Repeat on", Default: "monday", Options: []any{"everyday", "weekdays", "weekends"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: []string{"competitor-news-monitor"}, Tags: []string{"competitors", "news", "monitor", "research"},
	},
	{
		Key: "habit-checkin", Title: "Habit check-in", Category: "general",
		Description:      "A recurring nudge to keep a habit on track and reflect on whether you did it.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Nudge the user about their habit: {habit}. Ask whether they did it today, keep it warm and non-judgmental, and offer a one-line word of encouragement. One short message.",
		Slots:            []BlueprintSlot{{Name: "habit", Type: "text", Label: "Which habit?", Default: "20 minutes of reading", Strict: true}, blueprintTime("20:00"), {Name: "recurrence", Type: "weekdays", Label: "Repeat on", Default: "everyday", Options: []any{"everyday", "weekdays", "weekends"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"habit", "wellbeing"},
	},
	{
		Key: "hydration-move", Title: "Hydration & movement nudge", Category: "general",
		Description:      "A periodic nudge during the day to drink water, stand up, and stretch.",
		ScheduleTemplate: "0 {start_hour}-{end_hour}/{interval_hours} * * 1-5",
		PromptTemplate:   "Send the user a brief, friendly nudge to drink some water, stand up, and stretch for a moment. Vary the wording each time so it doesn't feel robotic. One short line.",
		Slots:            []BlueprintSlot{{Name: "interval_hours", Type: "enum", Label: "How often?", Default: "1", Options: []any{"1", "2", "3"}, Help: "hours between nudges", Strict: true}, {Name: "start_hour", Type: "enum", Label: "Start hour", Default: "9", Options: []any{"7", "8", "9", "10"}, Help: "first hour of the active window (24h)", Strict: true}, {Name: "end_hour", Type: "enum", Label: "End hour", Default: "17", Options: []any{"16", "17", "18", "19"}, Help: "last hour of the active window (24h)", Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"wellbeing", "focus"},
	},
	{
		Key: "meal-plan", Title: "Weekly meal plan", Category: "weekly",
		Description:      "A weekly meal plan plus a consolidated grocery list, tuned to your diet and how much time you have to cook.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Build the user a meal plan for the coming week: {meals} per day, suited to a {diet} diet and roughly {effort} cooking effort. Include a consolidated grocery list grouped by aisle. Keep blueprints simple and skimmable.",
		Slots:            []BlueprintSlot{{Name: "diet", Type: "enum", Label: "Diet?", Default: "no restrictions", Options: []any{"no restrictions", "vegetarian", "vegan", "high-protein", "low-carb"}, Strict: true}, {Name: "meals", Type: "enum", Label: "Meals per day?", Default: "dinner only", Options: []any{"dinner only", "lunch and dinner", "all three"}, Strict: true}, {Name: "effort", Type: "enum", Label: "Cooking effort?", Default: "quick", Options: []any{"quick", "medium", "ambitious"}, Strict: true}, blueprintTime("17:00"), {Name: "day", Type: "enum", Label: "Which day?", Default: "sunday", Options: []any{"sunday", "monday", "friday", "saturday"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"weekly", "food"},
	},
	{
		Key: "learn-daily", Title: "Daily learning drip", Category: "daily",
		Description:      "One bite-sized lesson a day on a topic you want to learn, building progressively over time.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Teach the user one bite-sized lesson about: {topic}. Build on earlier lessons so it progresses rather than repeating. Keep it to a couple of short paragraphs with one concrete example, and end with a single question to check understanding.",
		Slots:            []BlueprintSlot{{Name: "topic", Type: "text", Label: "Learn about…", Default: "Spanish vocabulary", Strict: true}, blueprintTime("08:30"), {Name: "recurrence", Type: "weekdays", Label: "Repeat on", Default: "weekdays", Options: []any{"everyday", "weekdays", "weekends"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"learning", "daily"},
	},
	{
		Key: "gratitude-journal", Title: "Gratitude & reflection prompt", Category: "general",
		Description:      "A gentle evening prompt to reflect on the day and note what went well.",
		ScheduleTemplate: "{minute} {hour} * * {dow}",
		PromptTemplate:   "Send the user a short, warm reflection prompt for the end of the day — invite them to note one thing that went well, one thing they are grateful for, and one small win. If they reply, acknowledge it kindly. One message.",
		Slots:            []BlueprintSlot{blueprintTime("21:30"), {Name: "recurrence", Type: "weekdays", Label: "Repeat on", Default: "everyday", Options: []any{"everyday", "weekdays", "weekends"}, Strict: true}, blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"wellbeing", "reflection"},
	},
	{
		Key: "on-this-day", Title: "On-this-day discovery", Category: "daily",
		Description:      "A daily dose of curiosity: a notable historical event, fact, or word for the day.",
		ScheduleTemplate: "{minute} {hour} * * *",
		PromptTemplate:   "Give the user one interesting '{flavor}' item for today — keep it short, surprising, and genuinely interesting. One or two sentences, no filler.",
		Slots:            []BlueprintSlot{{Name: "flavor", Type: "enum", Label: "What kind?", Default: "on this day in history", Options: []any{"on this day in history", "word of the day", "science fact", "quote of the day"}, Strict: true}, blueprintTime("07:30"), blueprintDeliver},
		DeliverDefault:   "origin", Skills: nil, Tags: []string{"daily", "curiosity"},
	},
}

// Time slot with the given default
func blueprintTime(def string) BlueprintSlot {
	return BlueprintSlot{Name: "time", Type: "time", Label: "What time?", Default: def, Help: "24h local time, e.g. 08:00", Strict: true}
}

var blueprintDeliver = BlueprintSlot{Name: "deliver", Type: "enum", Label: "Where to deliver?", Default: "origin", Options: []any{"origin", "local", "telegram", "discord", "email"}, Help: "origin = the chat you set this up from (or your configured home channel when created from the dashboard); local = save only, no message; or any connected platform name", Strict: false}

var weekdayPresetOrder = []string{"everyday", "weekdays", "weekends"}

var weekdayPresets = map[string]string{"everyday": "*", "weekdays": "1-5", "weekends": "0,6"}
