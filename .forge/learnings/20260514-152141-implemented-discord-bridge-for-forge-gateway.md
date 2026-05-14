# Learnings - 2026-05-14 15:21

- modernc.org/sqlite requires `?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)` in the DSN for WAL mode and busy timeout — the pragma syntax uses parentheses not equals signs
- When writing a text chunker that handles code fences, avoid re-scanning for fence boundaries from the full chunk window — this creates O(n²) behavior and can cause infinite loops. Instead, track fence state incrementally as you process the text.
- discordgo's `Identify.Intents` must be set before `Open()` — setting IntentsGuildMessages | IntentsGuildMessageReactions | IntentsGuilds covers thread/message/reaction events
- For distroless Docker images, don't use Dockerfile HEALTHCHECK since there's no shell or curl — let docker-compose handle health checking via the application's HTTP endpoint
- The `…` (ellipsis) character is 3 bytes in UTF-8 — when testing string length truncation, account for multi-byte characters in assertions
