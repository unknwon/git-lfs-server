package main

import (
	"log/slog"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	charmlog "charm.land/log/v2"

	"unknwon.dev/git-lfs-server/internal/logx"
)

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func setupLogging(cfg LogConfig) *logx.Logger {
	level := parseLevel(cfg.Level)
	handler := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		Level:           charmlog.Level(level),
		ReportTimestamp: true,
	})

	// Override warn level color to amber so it is less visually "green"-ish.
	styles := charmlog.DefaultStyles()
	styles.Levels[charmlog.WarnLevel] = lipgloss.NewStyle().
		SetString("WARN").
		Bold(true).
		Foreground(lipgloss.Color("226"))
	handler.SetStyles(styles)

	return logx.New(handler)
}
