package topstepx

import (
	"context"
	"log"
	"time"

	"github.com/benny-conn/brandon-bot/provider"
)

// CME futures schedule (ES, NQ, etc.):
//   Open:  Sunday 6:00 PM ET
//   Close: Friday 5:00 PM ET
//   Daily halt: 5:00 PM – 6:00 PM ET (Mon–Fri)
//
// So the market is "open" from 6:00 PM to 5:00 PM the next day, every day
// except the weekend gap (Friday 5 PM → Sunday 6 PM).

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

// isMarketOpen returns whether the CME futures market is currently in session.
func isMarketOpen(now time.Time) bool {
	wd := now.Weekday()
	hour := now.Hour()

	switch wd {
	case time.Saturday:
		return false
	case time.Sunday:
		// Open at 6 PM Sunday
		return hour >= 18
	case time.Friday:
		// Close at 5 PM Friday
		return hour < 17
	default: // Mon–Thu
		// Halt from 5 PM to 6 PM, otherwise open
		return hour < 17 || hour >= 18
	}
}

// nextSessionEvent returns the time and type of the next market_open or market_close
// event from the given time.
func nextSessionEvent(now time.Time) (time.Time, string) {
	wd := now.Weekday()
	hour := now.Hour()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	switch wd {
	case time.Saturday:
		// Next event: market_open Sunday 6 PM
		sunday := day.AddDate(0, 0, 1)
		return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 18, 0, 0, 0, now.Location()), "market_open"

	case time.Sunday:
		if hour < 18 {
			// Before Sunday open → next: market_open Sunday 6 PM
			return time.Date(day.Year(), day.Month(), day.Day(), 18, 0, 0, 0, now.Location()), "market_open"
		}
		// After Sunday open → next: market_close Monday 5 PM
		monday := day.AddDate(0, 0, 1)
		return time.Date(monday.Year(), monday.Month(), monday.Day(), 17, 0, 0, 0, now.Location()), "market_close"

	case time.Friday:
		if hour < 17 {
			// Before Friday close → next: market_close Friday 5 PM
			return time.Date(day.Year(), day.Month(), day.Day(), 17, 0, 0, 0, now.Location()), "market_close"
		}
		// After Friday close → next: market_open Sunday 6 PM
		sunday := day.AddDate(0, 0, 2)
		return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 18, 0, 0, 0, now.Location()), "market_open"

	default: // Mon–Thu
		if hour < 17 {
			// Before daily halt → next: market_close at 5 PM
			return time.Date(day.Year(), day.Month(), day.Day(), 17, 0, 0, 0, now.Location()), "market_close"
		}
		if hour < 18 {
			// During daily halt → next: market_open at 6 PM
			return time.Date(day.Year(), day.Month(), day.Day(), 18, 0, 0, 0, now.Location()), "market_open"
		}
		// After daily reopen → next: market_close tomorrow at 5 PM
		tomorrow := day.AddDate(0, 0, 1)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 17, 0, 0, 0, now.Location()), "market_close"
	}
}
