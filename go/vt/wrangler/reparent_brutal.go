package wrangler

import (
	"fmt"
	"time"

	"github.com/youtube/vitess/go/event"
	myproto "github.com/youtube/vitess/go/vt/mysqlctl/proto"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/topotools"
	"github.com/youtube/vitess/go/vt/topotools/events"
	"golang.org/x/net/context"
)

// reparentShardBrutal executes a brutal reparent.
//
// Assume the master is dead and not coming back. Just push your way
// forward.  Force means we are reparenting to the same master
// (assuming the data has been externally synched).
//
// The ev parameter is an event struct prefilled with information that the
// caller has on hand, which would be expensive for us to re-query.
func (wr *Wrangler) reparentShardBrutal(ctx context.Context, ev *events.Reparent, si *topo.ShardInfo, slaveTabletMap, masterTabletMap map[topo.TabletAlias]*topo.TabletInfo, masterElectTablet *topo.TabletInfo, leaveMasterReadOnly, force bool, waitSlaveTimeout time.Duration) (err error) {
	event.DispatchUpdate(ev, "starting brutal")

	defer func() {
		if err != nil {
			event.DispatchUpdate(ev, "failed: "+err.Error())
		}
	}()

	wr.logger.Infof("Skipping ValidateShard - not a graceful situation")

	if _, ok := slaveTabletMap[masterElectTablet.Alias]; !ok && !force {
		return fmt.Errorf("master elect tablet not in replication graph %v %v/%v %v", masterElectTablet.Alias, si.Keyspace(), si.ShardName(), topotools.MapKeys(slaveTabletMap))
	}

	// Check the master-elect and slaves are in good shape when the action
	// has not been forced.
	if !force {
		// Make sure all tablets have the right parent and reasonable positions.
		event.DispatchUpdate(ev, "checking slave replication positions")
		if err := wr.checkSlaveReplication(ctx, slaveTabletMap, topo.NO_TABLET, waitSlaveTimeout); err != nil {
			return err
		}

		// Check the master-elect is fit for duty - call out for hardware checks.
		event.DispatchUpdate(ev, "checking that new master is ready to serve")
		if err := wr.checkMasterElect(ctx, masterElectTablet); err != nil {
			return err
		}

		event.DispatchUpdate(ev, "checking slave consistency")
		wr.logger.Infof("check slaves %v/%v", masterElectTablet.Keyspace, masterElectTablet.Shard)
		restartableSlaveTabletMap := wr.restartableTabletMap(slaveTabletMap)
		err = wr.checkSlaveConsistency(ctx, restartableSlaveTabletMap, myproto.ReplicationPosition{}, waitSlaveTimeout)
		if err != nil {
			return err
		}
	} else {
		event.DispatchUpdate(ev, "stopping slave replication")
		wr.logger.Infof("forcing reparent to same master %v", masterElectTablet.Alias)
		err := wr.breakReplication(ctx, slaveTabletMap, masterElectTablet)
		if err != nil {
			return err
		}
	}

	event.DispatchUpdate(ev, "promoting new master")
	rsd, err := wr.promoteSlave(ctx, masterElectTablet)
	if err != nil {
		// FIXME(msolomon) This suggests that the master-elect is dead.
		// We need to classify certain errors as temporary and retry.
		return fmt.Errorf("promote slave failed: %v %v", err, masterElectTablet.Alias)
	}

	// Once the slave is promoted, remove it from our maps
	delete(slaveTabletMap, masterElectTablet.Alias)
	delete(masterTabletMap, masterElectTablet.Alias)

	event.DispatchUpdate(ev, "restarting slaves")
	majorityRestart, restartSlaveErr := wr.restartSlaves(ctx, slaveTabletMap, rsd)

	if !force {
		for _, failedMaster := range masterTabletMap {
			event.DispatchUpdate(ev, "scrapping old master")
			wr.logger.Infof("scrap dead master %v", failedMaster.Alias)
			// The master is dead so execute the action locally instead of
			// enqueing the scrap action for an arbitrary amount of time.
			if scrapErr := topotools.Scrap(ctx, wr.ts, failedMaster.Alias, false); scrapErr != nil {
				wr.logger.Warningf("scrapping failed master failed: %v", scrapErr)
			}
		}
	}

	event.DispatchUpdate(ev, "rebuilding shard serving graph")
	err = wr.finishReparent(ctx, si, masterElectTablet, majorityRestart, leaveMasterReadOnly)
	if err != nil {
		return err
	}

	event.DispatchUpdate(ev, "finished")

	if restartSlaveErr != nil {
		// This is more of a warning at this point.
		return restartSlaveErr
	}

	return nil
}
