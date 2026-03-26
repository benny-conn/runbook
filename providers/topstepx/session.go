package topstepx

import (
	"context"
	"log"
	"time"

	"github.com/benny-conn/brandon-bot/provider"
)

// TopStepX futures schedule:
//   Open:  Sunday 6:00 PM ET
//   Close: Friday 4:10 PM ET
//   Daily halt: 4:10 PM – 6:00 PM ET (Mon–Fri)
//
// So the market is "open" from 6:00 PM to 4:10 PM the next day, every day
// except the weekend gap (Friday 4:10 PM → Sunday 6 PM).

// SubscribeSession emits market_open and market_close events following the
// CME E-mini futures schedule. Implements provider.SessionNotifier.
func (p *Provider) SubscribeSession(ctx context.Context, handler func(provider.SessionEvent)) error {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return err
	}

	// Determine initial state and fire an immediate event if appropriate.
	now := time.Now().In(loc)
	if isMarketOpen(now) {
		handler(provider.SessionEvent{Type: "market_open", Time: now})
	} else {
		handler(provider.SessionEvent{Type: "market_close", Time: now})
	}

	for {
		now = time.Now().In(loc)
		nextEvent, eventType := nextSessionEvent(now)

		delay := time.Until(nextEvent)
		log.Printf("topstepx: next session event: %s at %s (in %s)",
			eventType, nextEvent.Format("Mon 2006-01-02 15:04 MST"), delay.Round(time.Second))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
			handler(provider.SessionEvent{
				Type: eventType,
				Time: nextEvent,
			})
		}
	}
}

// closeHour and closeMin define the daily TopStepX close time (4:10 PM ET).
const closeHour, closeMin = 16, 10

// isMarketOpen returns whether the TopStepX futures market is currently in session.
func isMarketOpen(now time.Time) bool {
	wd := now.Weekday()
	hhmm := now.Hour()*60 + now.Minute()
	closeAt := closeHour*60 + closeMin // 16:10 = 970

	switch wd {
	case time.Saturday:
		return false
	case time.Sunday:
		return hhmm >= 18*60 // open at 6 PM
	case time.Friday:
		return hhmm < closeAt
	default: // Mon–Thu
		return hhmm < closeAt || hhmm >= 18*60
	}
}

// nextSessionEvent returns the time and type of the next market_open or market_close
// event from the given time.
func nextSessionEvent(now time.Time) (time.Time, string) {
	wd := now.Weekday()
	hhmm := now.Hour()*60 + now.Minute()
	closeAt := closeHour*60 + closeMin
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	closeTime := time.Date(day.Year(), day.Month(), day.Day(), closeHour, closeMin, 0, 0, now.Location())
	openTime := time.Date(day.Year(), day.Month(), day.Day(), 18, 0, 0, 0, now.Location())

	switch wd {
	case time.Saturday:
		sunday := day.AddDate(0, 0, 1)
		return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 18, 0, 0, 0, now.Location()), "market_open"

	case time.Sunday:
		if hhmm < 18*60 {
			return openTime, "market_open"
		}
		monday := day.AddDate(0, 0, 1)
		return time.Date(monday.Year(), monday.Month(), monday.Day(), closeHour, closeMin, 0, 0, now.Location()), "market_close"

	case time.Friday:
		if hhmm < closeAt {
			return closeTime, "market_close"
		}
		sunday := day.AddDate(0, 0, 2)
		return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 18, 0, 0, 0, now.Location()), "market_open"

	default: // Mon–Thu
		if hhmm < closeAt {
			return closeTime, "market_close"
		}
		if hhmm < 18*60 {
			return openTime, "market_open"
		}
		tomorrow := day.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), closeHour, closeMin, 0, 0, now.Location()), "market_close"
	}
}
