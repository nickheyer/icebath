---
name: youtube-content
description: "YouTube transcripts to summaries, threads, blogs."
version: 1.0.0
author: Teknium (teknium1), icebath
license: MIT
platforms: [linux, macos, windows]
metadata:
  icebath:
    tags: [YouTube, Video, Transcripts, Media]
    related_skills: []
---

# YouTube Content Tool

## When to use

Use when the user shares a YouTube URL or video link, asks to summarize a video, requests a transcript, or wants to extract and reformat content from any YouTube video. Transforms transcripts into structured content (chapters, summaries, threads, blog posts).

Extract transcripts from YouTube videos and convert them into useful formats.

## Setup

Use `terminal` with a Python that imports `youtube_transcript_api`. When it
is missing and `icebath` is on PATH, let Icebath build a side environment for
the helper's one dependency under the Icebath home and print its interpreter;
do not install packages into Icebath's own dependency environment with raw pip
or project-discovering `uv run`:

```bash
python -c "import youtube_transcript_api" 2>/dev/null && PY=python ||
  PY="$(icebath pm environment youtube --requirement 'youtube-transcript-api>=1.2,<2')"
export PY
"$PY" -c "import youtube_transcript_api; print(youtube_transcript_api.__file__)"
```

In PowerShell: `$env:PY = icebath pm environment youtube --requirement 'youtube-transcript-api>=1.2,<2'`. If the terminal
runs on a different host or in a sandbox without Icebath, use an explicitly isolated helper
environment there (`python3 -m venv` plus `pip install 'youtube-transcript-api>=1.2,<2'`),
not the agent's own environment. Run every command
below with the interpreter whose import check succeeded (`"$PY"` in place of `python`).

## Helper Script

`SKILL_DIR` is the directory containing this SKILL.md file. The script accepts any standard YouTube URL format, short links (youtu.be), shorts, embeds, live links, or a raw 11-character video ID.

```bash
# JSON output with metadata
python SKILL_DIR/scripts/fetch_transcript.py "https://youtube.com/watch?v=VIDEO_ID"

# Plain text (good for piping into further processing)
python SKILL_DIR/scripts/fetch_transcript.py "URL" --text-only

# With timestamps
python SKILL_DIR/scripts/fetch_transcript.py "URL" --timestamps

# Specific language with fallback chain
python SKILL_DIR/scripts/fetch_transcript.py "URL" --language tr,en
```

## Output Formats

After fetching the transcript, format it based on what the user asks for:

- **Chapters**: Group by topic shifts, output timestamped chapter list
- **Summary**: Concise 5-10 sentence overview of the entire video
- **Chapter summaries**: Chapters with a short paragraph summary for each
- **Thread**: Twitter/X thread format — numbered posts, each under 280 chars
- **Blog post**: Full article with title, sections, and key takeaways
- **Quotes**: Notable quotes with timestamps

### Example — Chapters Output

```
00:00 Introduction — host opens with the problem statement
03:45 Background — prior work and why existing solutions fall short
12:20 Core method — walkthrough of the proposed approach
24:10 Results — benchmark comparisons and key takeaways
31:55 Q&A — audience questions on scalability and next steps
```

## Workflow

1. **Fetch** the transcript using `terminal` and the prepared Python with `--text-only --timestamps`.
2. **Validate**: confirm the output is non-empty and in the expected language. If empty, retry without `--language` to get any available transcript. If still empty, tell the user the video likely has transcripts disabled.
3. **Chunk if needed**: if the transcript exceeds ~50K characters, split into overlapping chunks (~40K with 2K overlap) and summarize each chunk before merging.
4. **Transform** into the requested output format. If the user did not specify a format, default to a summary.
5. **Verify**: re-read the transformed output to check for coherence, correct timestamps, and completeness before presenting.

## Error Handling

- **Transcript disabled**: tell the user; suggest they check if subtitles are available on the video page.
- **Private/unavailable video**: relay the error and ask the user to verify the URL.
- **No matching language**: retry without `--language` to fetch any available transcript, then note the actual language to the user.
- **Dependency missing**: repeat PM preparation and reactivation above, then verify the helper uses that Python. Do not repair the selected generation with pip.
