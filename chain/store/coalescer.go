package store

import (
	"context"
	"time"

	"github.com/filecoin-project/lotus/chain/types"
)

// WrapHeadChangeCoalescer wraps a ReorgNotifee with a head change coalescer.
func WrapHeadChangeCoalescer(fn ReorgNotifee, delay time.Duration) ReorgNotifee {
	c := NewHeadChangeCoalescer(fn, delay)
	return c.HeadChange
}

// HeadChangeCoalescer is a stateful reorg notifee which coalesces incoming head changes
// with pending head changes to reduce state computations from head change notifications.
type HeadChangeCoalescer struct {
	notify ReorgNotifee

	ctx    context.Context
	cancel func()

	eventq chan headChange

	revert []*types.TipSet
	apply  []*types.TipSet
}

type headChange struct {
	revert, apply []*types.TipSet
}

// NewHeadChangeCoalescer creates a HeadChangeCoalescer.
func NewHeadChangeCoalescer(fn ReorgNotifee, delay time.Duration) *HeadChangeCoalescer {
	ctx, cancel := context.WithCancel(context.Background())
	c := &HeadChangeCoalescer{
		notify: fn,
		ctx:    ctx,
		cancel: cancel,
		eventq: make(chan headChange),
	}

	go c.background(delay)

	return c
}

// HeadChange is the ReorgNotifee callback for the stateful coalescer; it receives an incoming
// head change and schedules dispatch of a coalesced head change in the background.
func (c *HeadChangeCoalescer) HeadChange(revert, apply []*types.TipSet) error {
	select {
	case c.eventq <- headChange{revert: revert, apply: apply}:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

// Close closes the coalescer and cancels the background dispatch goroutine.
// Any further notification will result in an error.
func (c *HeadChangeCoalescer) Close() error {
	select {
	case <-c.ctx.Done():
	default:
		c.cancel()
	}

	return nil
}

// Implementation details

func (c *HeadChangeCoalescer) background(delay time.Duration) {
	var timerC <-chan time.Time
	for {
		select {
		case evt := <-c.eventq:
			c.coalesce(evt.revert, evt.apply)
			if timerC == nil {
				timerC = time.After(delay)
			}

		case <-timerC:
			c.dispatch()
			timerC = nil

		case <-c.ctx.Done():
			if c.revert != nil || c.apply != nil {
				c.dispatch()
			}
			return
		}
	}
}

func (c *HeadChangeCoalescer) coalesce(revert, apply []*types.TipSet) {
	// newly reverted tipsets cancel out with pending applys.
	// similarly, newly applied tipsets cancel out with pending reverts.

	// pending tipsets
	pendRevert := make(map[types.TipSetKey]struct{}, len(c.revert))
	for _, ts := range c.revert {
		pendRevert[ts.Key()] = struct{}{}
	}

	pendApply := make(map[types.TipSetKey]struct{}, len(c.apply))
	for _, ts := range c.apply {
		pendApply[ts.Key()] = struct{}{}
	}

	// incoming tipsets
	reverting := make(map[types.TipSetKey]struct{}, len(revert))
	for _, ts := range revert {
		reverting[ts.Key()] = struct{}{}
	}

	applying := make(map[types.TipSetKey]struct{}, len(apply))
	for _, ts := range apply {
		applying[ts.Key()] = struct{}{}
	}

	// coalesced revert set
	// - pending reverts are cancelled by incoming applys
	// - incoming reverts are cancelled by pending applys
	newRevert := make([]*types.TipSet, 0, len(c.revert)+len(revert))
	for _, ts := range c.revert {
		_, cancel := applying[ts.Key()]
		if cancel {
			continue
		}

		newRevert = append(newRevert, ts)
	}

	for _, ts := range revert {
		_, cancel := pendApply[ts.Key()]
		if cancel {
			continue
		}

		newRevert = append(newRevert, ts)
	}

	// coalesced apply set
	// - pending applys are cancelled by incoming reverts
	// - incoming applys are cancelled by pending reverts
	newApply := make([]*types.TipSet, 0, len(c.apply)+len(apply))
	for _, ts := range c.apply {
		_, cancel := reverting[ts.Key()]
		if cancel {
			continue
		}

		newApply = append(newApply, ts)
	}

	for _, ts := range apply {
		_, cancel := pendRevert[ts.Key()]
		if cancel {
			continue
		}

		newApply = append(newApply, ts)
	}

	// commit the coalesced sets
	c.revert = newRevert
	c.apply = newApply
}

func (c *HeadChangeCoalescer) dispatch() {
	err := c.notify(c.revert, c.apply)
	if err != nil {
		log.Errorf("error dispatching coalesced head change notification: %s", err)
	}

	c.revert = nil
	c.apply = nil
}
