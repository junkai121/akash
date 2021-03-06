package bidengine

import (
	lifecycle "github.com/boz/go-lifecycle"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ovrclk/akash/provider/cluster"
	"github.com/ovrclk/akash/provider/event"
	"github.com/ovrclk/akash/provider/session"
	"github.com/ovrclk/akash/pubsub"
	"github.com/ovrclk/akash/util/runner"
	dquery "github.com/ovrclk/akash/x/deployment/query"
	mquery "github.com/ovrclk/akash/x/market/query"
	mtypes "github.com/ovrclk/akash/x/market/types"
	"github.com/tendermint/tendermint/libs/log"
)

// order manages bidding and general lifecycle handling of an order.
type order struct {
	order mtypes.OrderID
	bid   *mquery.Bid

	session session.Session
	cluster cluster.Cluster
	bus     pubsub.Bus
	sub     pubsub.Subscriber

	log log.Logger
	lc  lifecycle.Lifecycle
}

func newOrder(e *service, oid mtypes.OrderID, bid *mquery.Bid) (*order, error) {

	// Create a subscription that will see all events that have not been read from e.sub.Events()
	sub, err := e.sub.Clone()
	if err != nil {
		return nil, err
	}

	session := e.session.ForModule("bidengine-order")

	log := session.Log().With("order", oid)

	order := &order{
		order:   oid,
		bid:     bid,
		session: session,
		cluster: e.cluster,
		bus:     e.bus,
		sub:     sub,
		log:     log,
		lc:      lifecycle.New(),
	}

	// Shut down when parent begins shutting down
	go order.lc.WatchChannel(e.lc.ShuttingDown())

	// Run main loop in separate thread.
	go order.run()

	// Notify parent of completion (allows drain).
	go func() {
		<-order.lc.Done()
		e.drainch <- order
	}()

	return order, nil
}

func (o *order) run() {
	defer o.lc.ShutdownCompleted()

	var (
		// channels for async operations.
		groupch   <-chan runner.Result
		clusterch <-chan runner.Result
		bidch     <-chan runner.Result

		group       *dquery.Group
		reservation cluster.Reservation

		won bool
	)

	// Begin fetching group details immediately.
	groupch = runner.Do(func() runner.Result {
		return runner.NewResult(
			o.session.Client().Query().Group(o.order.GroupID()))
	})

loop:
	for {
		select {
		case <-o.lc.ShutdownRequest():
			break loop

		case ev := <-o.sub.Events():
			switch ev := ev.(type) {
			case mtypes.EventLeaseCreated:

				// different group
				if !o.order.GroupID().Equals(ev.ID.GroupID()) {
					o.log.Debug("ignoring group", "group", ev.ID.GroupID())
					break
				}

				// check winning provider
				if !ev.ID.Provider.Equals(o.session.Provider().Address()) {
					o.log.Info("lease lost", "lease", ev.ID)
					break loop
				}

				// TODO: sanity check (price, state, etc...)

				o.log.Info("lease won", "lease", ev.ID)

				o.bus.Publish(event.LeaseWon{
					LeaseID: ev.ID,
					Group:   group,
					// Price:   ev.Price,
				})
				won = true

				break loop

			case mtypes.EventOrderClosed:

				// different deployment
				if !ev.ID.Equals(o.order) {
					break
				}

				o.log.Info("order closed")
				break loop
			}

		case result := <-groupch:
			// Group details fetched.

			groupch = nil
			o.log.Info("group fetched")

			if result.Error() != nil {
				o.log.Error("fetching group", "err", result.Error())
				break loop
			}

			res := result.Value().(dquery.Group)
			group = &res

			if !o.shouldBid(group) {
				break
			}

			// Begin reserving resources from cluster.
			clusterch = runner.Do(func() runner.Result {
				return runner.NewResult(o.cluster.Reserve(o.order, group))
			})

		case result := <-clusterch:
			clusterch = nil
			o.log.Info("reserve requested")

			if result.Error() != nil {
				o.log.Error("reserving resources", "err", result.Error())
				break loop
			}

			if o.bid != nil {
				// fulfillment already created (state recovered via queryExistingOrders)
				break
			}

			// Resources reservied.  Calculate price and bid.

			reservation = result.Value().(cluster.Reservation)

			// TODO: price
			// price := calculatePrice(reservation.Resources())
			price := sdk.NewCoin("akash", sdk.NewInt(0))

			o.log.Debug("submitting fulfillment", "price", price)

			// Begin submitting fulfillment
			bidch = runner.Do(func() runner.Result {
				return runner.NewResult(nil, o.session.Client().Tx().Broadcast(&mtypes.MsgCreateBid{
					Order:    o.order,
					Provider: o.session.Provider().Address(),
					// TODO: price
					// Price:    price,
				}))
			})

		case result := <-bidch:
			bidch = nil
			o.log.Info("bid complete")

			if result.Error() != nil {
				o.log.Error("submitting fulfillment", "err", result.Error())
				break loop
			}

			// Fulfillment placed.  All done.
		}
	}

	o.log.Info("shutting down")
	o.lc.ShutdownInitiated(nil)
	o.sub.Close()

	// cancel reservation
	if !won && reservation != nil {
		o.log.Debug("unreserving reservation")
		if err := o.cluster.Unreserve(reservation.OrderID(), reservation.Resources()); err != nil {
			o.log.Error("error unreserving reservation", "err", err)
		}
	}

	// Wait for all runners to complete.
	if groupch != nil {
		<-groupch
	}
	if clusterch != nil {
		<-clusterch
	}
	if bidch != nil {
		<-bidch
	}
}

func (o *order) shouldBid(group *dquery.Group) bool {

	// does provider have required attributes?
	if !group.MatchAttributes(o.session.Provider().Attributes) {
		o.log.Debug("unable to fulfill: incompatible attributes")
		return false
	}

	// XXX
	// if err := validation.ValidateDeploymentGroup(group); err != nil {
	// 	o.log.Error("unable to fulfill: group validation error",
	// 		"err", err)
	// 	return false
	// }
	return true
}
