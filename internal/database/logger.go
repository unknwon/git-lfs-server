package database

import (
	"fmt"
	"time"

	gormlogger "gorm.io/gorm/logger"

	"unknwon.dev/git-lfs-server/internal/logx"
)

// logxWriter adapts *logx.Logger to GORM's logger.Writer interface so that
// database log output flows through the application's structured logging pipeline.
type logxWriter struct{ l *logx.Logger }

func (w logxWriter) Printf(msg string, args ...any) {
	w.l.Debug(fmt.Sprintf(msg, args...))
}

func newGORMLogger(l *logx.Logger, slowThreshold time.Duration) gormlogger.Interface {
	return gormlogger.New(logxWriter{l}, gormlogger.Config{
		SlowThreshold:             slowThreshold,
		LogLevel:                  gormlogger.Info,
		IgnoreRecordNotFoundError: true,
	})
}
