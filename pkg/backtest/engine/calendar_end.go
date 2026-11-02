package engine

import (
	"fmt"
	"time"
)

// CalendarEndError is a Sessions read past the last session of the
// engine's calendar, which is loaded about a year ahead. LastSession is that
// session, at midnight UTC.
type CalendarEndError struct {
	Calendar    string
	LastSession time.Time
}

func (e *CalendarEndError) Error() string {
	return fmt.Sprintf("the %s calendar ends on %s", e.Calendar, e.LastSession.Format(time.DateOnly))
}
