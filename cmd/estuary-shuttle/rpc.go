package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/application-research/estuary/drpc"
	"github.com/application-research/estuary/pinner"
	"github.com/application-research/estuary/pinner/types"
	"github.com/application-research/estuary/util"
	dagsplit "github.com/application-research/estuary/util/dagsplit"
	"github.com/application-research/filclient"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-state-types/abi"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	"github.com/ipfs/go-merkledag"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/xerrors"
	"gorm.io/gorm"
)

func (d *Shuttle) handleRpcCmd(cmd *drpc.Command) error {
	ctx := context.TODO()

	// If the command contains a trace continue it here.
	if cmd.HasTraceCarrier() {
		if sc := cmd.TraceCarrier.AsSpanContext(); sc.IsValid() {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}
	}

	log.Debugf("handling rpc command: %s", cmd.Op)
	switch cmd.Op {
	case drpc.CMD_AddPin:
		return d.handleRpcAddPin(ctx, cmd.Params.AddPin)
	case drpc.CMD_ComputeCommP:
		return d.handleRpcComputeCommP(ctx, cmd.Params.ComputeCommP)
	case drpc.CMD_TakeContent:
		return d.handleRpcTakeContent(ctx, cmd.Params.TakeContent)
	case drpc.CMD_AggregateContent:
		return d.handleRpcAggregateStagedContent(ctx, cmd.Params.AggregateContent)
	case drpc.CMD_StartTransfer:
		return d.handleRpcStartTransfer(ctx, cmd.Params.StartTransfer)
	case drpc.CMD_PrepareForDataRequest:
		return d.handleRpcPrepareForDataRequest(ctx, cmd.Params.PrepareForDataRequest)
	case drpc.CMD_CleanupPreparedRequest:
		return d.handleRpcCleanupPreparedRequest(ctx, cmd.Params.CleanupPreparedRequest)
	case drpc.CMD_ReqTxStatus:
		return d.handleRpcReqTxStatus(ctx, cmd.Params.ReqTxStatus)
	case drpc.CMD_RetrieveContent:
		return d.handleRpcRetrieveContent(ctx, cmd.Params.RetrieveContent)
	case drpc.CMD_UnpinContent:
		return d.handleRpcUnpinContent(ctx, cmd.Params.UnpinContent)
	case drpc.CMD_SplitContent:
		return d.handleRpcSplitContent(ctx, cmd.Params.SplitContent)
	case drpc.CMD_RestartTransfer:
		return d.handleRpcRestartTransfer(ctx, cmd.Params.RestartTransfer)
	default:
		return fmt.Errorf("unrecognized command op: %q", cmd.Op)
	}
}

func (d *Shuttle) sendRpcMessage(ctx context.Context, msg *drpc.Message) error {
	// if a span is contained in `ctx` its SpanContext will be carried in the message, otherwise
	// a noopspan context will be carried and ignored by the receiver.
	msg.TraceCarrier = drpc.NewTraceCarrier(trace.SpanFromContext(ctx).SpanContext())
	log.Debugf("sending rpc message: %s", msg.Op)
	select {
	case d.outgoing <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Shuttle) handleRpcAddPin(ctx context.Context, apo *drpc.AddPin) error {
	d.addPinLk.Lock()
	defer d.addPinLk.Unlock()
	return d.addPin(ctx, apo.DBID, apo.Cid, apo.UserId, apo.Peers, false)
}

func (d *Shuttle) addPin(ctx context.Context, contid uint, data cid.Cid, user uint, peers []*peer.AddrInfo, skipLimiter bool) error {
	ctx, span := d.Tracer.Start(ctx, "addPin", trace.WithAttributes(
		attribute.Int64("contID", int64(contid)),
		attribute.Int64("userID", int64(user)),
		attribute.String("data", data.String()),
		attribute.Bool("skipLimiter", skipLimiter),
	))
	defer span.End()

	var search []Pin
	if err := d.DB.Find(&search, "content = ?", contid).Error; err != nil {
		return err
	}

	if len(search) > 0 {
		// already have a pin with this content id
		if len(search) > 1 {
			log.Errorf("have multiple pins for same content id: %d", contid)
		}
		existing := search[0]

		if existing.Failed {
			// being asked to pin a thing we have marked as failed means the
			// primary node isnt aware that this pin failed, we need to resend
			// that notification

			if err := d.sendRpcMessage(ctx, &drpc.Message{
				Op: drpc.OP_UpdatePinStatus,
				Params: drpc.MsgParams{
					UpdatePinStatus: &drpc.UpdatePinStatus{
						DBID:   contid,
						Status: types.PinningStatusFailed,
					},
				},
			}); err != nil {
				log.Errorf("failed to send pin status update: %s", err)
			}
			return nil
		}

		if !existing.Pinning && existing.Active {
			// we already finished pinning this one
			// This implies that the pin complete message got lost, need to resend all the objects

			go func() {
				if err := d.resendPinComplete(ctx, existing); err != nil {
					log.Error(err)
				}
			}()
			return nil
		}

		if !existing.Active && !existing.Pinning {
			if err := d.DB.Model(Pin{}).Where("id = ?", existing.ID).UpdateColumn("pinning", true).Error; err != nil {
				return xerrors.Errorf("failed to update pin pinning state to true: %s", err)
			}
		}
	} else {
		// good, no pin found with this content id, lets create it
		pin := &Pin{
			Content: contid,
			Cid:     util.DbCID{CID: data},
			UserID:  user,
			Active:  false,
			Pinning: true,
		}

		if err := d.DB.Create(pin).Error; err != nil {
			return err
		}
	}

	op := &pinner.PinningOperation{
		Obj:         data,
		ContId:      contid,
		UserId:      user,
		Status:      types.PinningStatusQueued,
		SkipLimiter: skipLimiter,
		Peers:       peers,
	}

	d.PinMgr.Add(op)
	return nil
}

func (s *Shuttle) resendPinComplete(ctx context.Context, pin Pin) error {
	objects, err := s.objectsForPin(ctx, pin.ID)
	if err != nil {
		return fmt.Errorf("failed to get objects for pin: %s", err)
	}

	s.sendPinCompleteMessage(ctx, pin.Content, pin.Size, objects)
	return nil
}

func (s *Shuttle) objectsForPin(ctx context.Context, pin uint) ([]*Object, error) {
	_, span := s.Tracer.Start(ctx, "objectsForPin")
	defer span.End()

	var objects []*Object
	if err := s.DB.Model(ObjRef{}).Where("pin = ?", pin).
		Joins("left join objects on obj_refs.object = objects.id").
		Select("objects.*").
		Scan(&objects).Error; err != nil {
		return nil, err
	}

	return objects, nil
}

type commpResult struct {
	CommP   cid.Cid
	Size    abi.UnpaddedPieceSize
	CarSize uint64
}

func (d *Shuttle) handleRpcComputeCommP(ctx context.Context, cmd *drpc.ComputeCommP) error {
	ctx, span := d.Tracer.Start(ctx, "handleComputeCommP", trace.WithAttributes(
		attribute.String("data", cmd.Data.String()),
	))
	defer span.End()

	res, err := d.commpMemo.Do(ctx, cmd.Data.String(), nil)
	if err != nil {
		return xerrors.Errorf("failed to compute commP for %s: %w", cmd.Data, err)
	}

	commpRes, ok := res.(*commpResult)
	if !ok {
		return xerrors.Errorf("result from commp memoizer was of wrong type: %T", res)
	}

	return d.sendRpcMessage(ctx, &drpc.Message{
		Op: drpc.OP_CommPComplete,
		Params: drpc.MsgParams{
			CommPComplete: &drpc.CommPComplete{
				Data:    cmd.Data,
				CommP:   commpRes.CommP,
				CarSize: commpRes.CarSize,
				Size:    commpRes.Size,
			},
		},
	})
}

func (s *Shuttle) sendSplitContentComplete(ctx context.Context, cont uint) {
	if err := s.sendRpcMessage(ctx, &drpc.Message{
		Op: drpc.OP_SplitComplete,
		Params: drpc.MsgParams{
			SplitComplete: &drpc.SplitComplete{
				ID: cont,
			},
		},
	}); err != nil {
		log.Errorf("failed to send split content complete message: %s", err)
	}
}

func (d *Shuttle) sendPinCompleteMessage(ctx context.Context, cont uint, size int64, objects []*Object) {
	ctx, span := d.Tracer.Start(ctx, "sendPinCompleteMessage")
	defer span.End()

	objs := make([]drpc.PinObj, 0, len(objects))
	for _, o := range objects {
		objs = append(objs, drpc.PinObj{
			Cid:  o.Cid.CID,
			Size: o.Size,
		})
	}

	if err := d.sendRpcMessage(ctx, &drpc.Message{
		Op: drpc.OP_PinComplete,
		Params: drpc.MsgParams{
			PinComplete: &drpc.PinComplete{
				DBID:    cont,
				Size:    size,
				Objects: objs,
			},
		},
	}); err != nil {
		log.Errorf("failed to send pin complete message for content %d: %s", cont, err)
	}
}

// handleRpcTakeContent is used for consolidation, pulls pins from one shuttle to another shuttle
func (d *Shuttle) handleRpcTakeContent(ctx context.Context, cmd *drpc.TakeContent) error {
	ctx, span := d.Tracer.Start(ctx, "handleTakeContent")
	defer span.End()

	d.addPinLk.Lock()
	defer d.addPinLk.Unlock()

	for _, c := range cmd.Contents {
		var count int64
		err := d.DB.Model(Pin{}).Where("content = ?", c.ID).Limit(1).Count(&count).Error
		if err != nil {
			return err
		}

		// if this content is already in this shuttle, ignore pinning it
		if count > 0 {
			continue
		}

		go func(c drpc.ContentFetch) {
			if err := d.addPin(ctx, c.ID, c.Cid, c.UserID, c.Peers, true); err != nil {
				log.Errorf("failed to pin takeContent: %d", c.ID)
			}
		}(c)
	}
	return nil
}

func (s *Shuttle) handleRpcAggregateStagedContent(ctx context.Context, cmd *drpc.AggregateContent) error {
	// only progress if aggr is not allready in progress
	if !s.markStartAggr(cmd.DBID) {
		return nil
	}
	defer s.finishAggr(cmd.DBID)

	ctx, span := s.Tracer.Start(ctx, "handleAggregateContent", trace.WithAttributes(
		attribute.Int64("dbID", int64(cmd.DBID)),
		attribute.Int64("userId", int64(cmd.UserID)),
		attribute.String("root", cmd.Root.String()),
	))
	defer span.End()

	var p Pin
	err := s.DB.First(&p, "content = ?", cmd.DBID).Error
	switch err {
	default:
		return err
	case nil:
		// exists already
		return nil
	case gorm.ErrRecordNotFound:
		// normal case
	}

	totalSize := int64(len(cmd.ObjData))
	for _, c := range cmd.Contents {
		var aggr Pin
		if err := s.DB.First(&aggr, "content = ?", c).Error; err != nil {
			// TODO: implies we dont have all the content locally we are being
			// asked to aggregate, this is an important error to handle
			return err
		}

		if !aggr.Active || aggr.Failed {
			return fmt.Errorf("content i am being asked to aggregate is not pinned: %d", c)
		}
		totalSize += aggr.Size
	}

	pin := &Pin{
		Content:   cmd.DBID,
		Cid:       util.DbCID{CID: cmd.Root},
		UserID:    cmd.UserID,
		Size:      totalSize,
		Active:    false,
		Pinning:   true,
		Aggregate: true,
	}
	if err := s.DB.Create(pin).Error; err != nil {
		return err
	}

	blk, err := blocks.NewBlockWithCid(cmd.ObjData, cmd.Root)
	if err != nil {
		return err
	}

	if err := s.Node.Blockstore.Put(ctx, blk); err != nil {
		return err
	}

	// since aggregates only needs put the containing box in the blockstore (no need to pull blocks),
	// mark it as active and change pinning status
	if err := s.DB.Model(Pin{}).Where("id = ?", pin.ID).UpdateColumns(map[string]interface{}{
		"active":  true,
		"pinning": false,
	}).Error; err != nil {
		return err
	}

	obj := &Object{
		Cid:  util.DbCID{CID: blk.Cid()},
		Size: len(blk.RawData()),
	}
	if err := s.DB.Create(obj).Error; err != nil {
		return err
	}

	ref := &ObjRef{
		Pin:    pin.ID,
		Object: obj.ID,
	}
	if err := s.DB.Create(ref).Error; err != nil {
		return err
	}

	if err := s.DB.Model(Pin{}).Where("id = ?", pin.ID).UpdateColumns(map[string]interface{}{
		"active":  true,
		"pinning": false,
	}).Error; err != nil {
		return err
	}

	s.sendPinCompleteMessage(ctx, cmd.DBID, totalSize, []*Object{obj})
	return nil
}

func (s *Shuttle) trackTransfer(chanid *datatransfer.ChannelID, dealdbid uint, st *filclient.ChannelState) {
	s.tcLk.Lock()
	defer s.tcLk.Unlock()

	s.trackingChannels[chanid.String()] = &util.ChanTrack{
		Dbid: dealdbid,
		Last: st,
	}
}

func (s *Shuttle) handleRpcPrepareForDataRequest(ctx context.Context, cmd *drpc.PrepareForDataRequest) error {
	ctx, span := s.Tracer.Start(ctx, "handleRpcPrepareForDataRequest", trace.WithAttributes(
		attribute.Int64("dealDbID", int64(cmd.DealDBID)),
		attribute.String("proposalCID", cmd.ProposalCid.String()),
		attribute.String("payloadCID", cmd.PayloadCid.String()),
		attribute.Int64("size", int64(cmd.Size)),
	))
	defer span.End()

	// Tell server to prepare to receive a new pull transfer
	err := s.Filc.Libp2pTransferMgr.PrepareForDataRequest(ctx, cmd.DealDBID, cmd.AuthToken, cmd.ProposalCid, cmd.PayloadCid, cmd.Size)
	if err != nil {
		return fmt.Errorf("preparing for data request: %w", err)
	}
	return nil
}

func (s *Shuttle) handleRpcCleanupPreparedRequest(ctx context.Context, cmd *drpc.CleanupPreparedRequest) error {
	ctx, span := s.Tracer.Start(ctx, "handleRpcCleanupPreparedRequest", trace.WithAttributes(
		attribute.Int64("dealDbID", int64(cmd.DealDBID)),
	))
	defer span.End()

	// Tell server to clean up auth token and cancel any running transfer
	err := s.Filc.Libp2pTransferMgr.CleanupPreparedRequest(ctx, cmd.DealDBID, cmd.AuthToken)
	if err != nil {
		return fmt.Errorf("cleaning up prepared request: %w", err)
	}
	return nil
}

func (s *Shuttle) handleRpcStartTransfer(ctx context.Context, cmd *drpc.StartTransfer) error {
	ctx, span := s.Tracer.Start(ctx, "handleStartTransfer", trace.WithAttributes(
		attribute.Int64("contentID", int64(cmd.ContentID)),
		attribute.Int64("dealDbID", int64(cmd.DealDBID)),
		attribute.String("miner", cmd.Miner.String()),
		attribute.String("dataCID", cmd.DataCid.String()),
		attribute.String("propCID", cmd.PropCid.String()),
	))
	defer span.End()

	chanid, err := s.Filc.StartDataTransfer(ctx, cmd.Miner, cmd.PropCid, cmd.DataCid)
	if err != nil {
		s.sendTransferStatusUpdate(ctx, &drpc.TransferStatus{
			DealDBID: cmd.DealDBID,
			Failed:   true,
			Message:  fmt.Sprintf("failed to start data transfer: %s", err),
		})
		return err
	}
	s.trackTransfer(chanid, cmd.DealDBID, nil)
	return nil
}

func (d *Shuttle) sendTransferStatusUpdate(ctx context.Context, st *drpc.TransferStatus) {
	ctx, span := d.Tracer.Start(ctx, "sendTransferStatusUpdate")
	defer span.End()

	var extra string
	if st.State != nil {
		extra = fmt.Sprintf("%d %s", st.State.Status, st.State.Message)
	}

	log.Debugf("sending transfer status update: %d %s", st.DealDBID, extra)
	if err := d.sendRpcMessage(ctx, &drpc.Message{
		Op: drpc.OP_TransferStatus,
		Params: drpc.MsgParams{
			TransferStatus: st,
		},
	}); err != nil {
		log.Errorf("failed to send transfer status update: %s", err)
	}
}

func (s *Shuttle) handleRpcReqTxStatus(ctx context.Context, req *drpc.ReqTxStatus) error {
	_, span := s.Tracer.Start(ctx, "handleReqTxStatus", trace.WithAttributes(
		attribute.Int64("dealDbID", int64(req.DealDBID)),
	))
	defer span.End()

	ctx = context.TODO()
	st, err := s.Filc.TransferStatusByID(ctx, req.ChanID)
	if err != nil && err != filclient.ErrNoTransferFound && !strings.Contains(err.Error(), "No channel for channel ID") && !strings.Contains(err.Error(), "datastore: key not found") {
		return err
	}

	if st == nil {
		return nil
	}

	trsFailed, msg := util.TransferFailed(st)
	s.sendTransferStatusUpdate(ctx, &drpc.TransferStatus{
		Chanid:   req.ChanID,
		DealDBID: req.DealDBID,
		State:    st,
		Failed:   trsFailed,
		Message:  fmt.Sprintf("status: %d(%s), message: %s", st.Status, msg, st.Message),
	})
	return nil
}

func (s *Shuttle) handleRpcRetrieveContent(ctx context.Context, req *drpc.RetrieveContent) error {
	return s.retrieveContent(ctx, req)
}

func (s *Shuttle) handleRpcUnpinContent(ctx context.Context, req *drpc.UnpinContent) error {
	for _, c := range req.Contents {
		go func(cntID uint) {
			if err := s.Unpin(ctx, cntID); err != nil {
				log.Errorf("failed to unpin content %d: %s", cntID, err)
			}
		}(c)
	}
	return nil
}

func (s *Shuttle) markStartUnpin(cont uint) bool {
	s.unpinLk.Lock()
	defer s.unpinLk.Unlock()

	if s.unpinInProgress[cont] {
		return false
	}

	s.unpinInProgress[cont] = true
	return true
}

func (s *Shuttle) finishUnpin(cont uint) {
	s.unpinLk.Lock()
	defer s.unpinLk.Unlock()
	delete(s.unpinInProgress, cont)
}

func (s *Shuttle) markStartAggr(cont uint) bool {
	s.aggrLk.Lock()
	defer s.aggrLk.Unlock()

	if s.aggrInProgress[cont] {
		return false
	}

	s.aggrInProgress[cont] = true
	return true
}

func (s *Shuttle) finishAggr(cont uint) {
	s.aggrLk.Lock()
	defer s.aggrLk.Unlock()
	delete(s.aggrInProgress, cont)
}

func (s *Shuttle) markStartSplit(cont uint) bool {
	s.splitLk.Lock()
	defer s.splitLk.Unlock()

	if s.splitsInProgress[cont] {
		return false
	}

	s.splitsInProgress[cont] = true
	return true
}

func (s *Shuttle) finishSplit(cont uint) {
	s.splitLk.Lock()
	defer s.splitLk.Unlock()
	delete(s.splitsInProgress, cont)
}

func (s *Shuttle) handleRpcSplitContent(ctx context.Context, req *drpc.SplitContent) error {
	// only progress if split is not allready in progress
	if !s.markStartSplit(req.Content) {
		return nil
	}
	defer s.finishSplit(req.Content)

	var pin Pin
	if err := s.DB.First(&pin, "content = ?", req.Content).Error; err != nil {
		return xerrors.Errorf("no pin with content %d found for split content request: %w", req.Content, err)
	}

	// Check if we've done this already...
	if pin.DagSplit {
		// This is only set once we've completed the splitting, so this should be fine
		if pin.SplitFrom != 0 {
			return fmt.Errorf("was asked to split content that is itself split from a larger piece")
		}

		// However, this might mean that the other side missed receiving some
		// messages from us, so we need to resend everything
		var children []Pin
		if err := s.DB.Find(&children, "split_from = ?", req.Content).Error; err != nil {
			return err
		}

		for _, c := range children {
			if err := s.resendPinComplete(ctx, c); err != nil {
				return err
			}
		}
		s.sendSplitContentComplete(ctx, pin.Content)
		return nil
	}

	dserv := merkledag.NewDAGService(blockservice.New(s.Node.Blockstore, nil))
	b := dagsplit.NewBuilder(dserv, uint64(req.Size), 0)
	if err := b.Pack(ctx, pin.Cid.CID); err != nil {
		return err
	}

	cst := cbor.NewCborStore(s.Node.Blockstore)

	var boxCids []cid.Cid
	for _, box := range b.Boxes() {
		cc, err := cst.Put(ctx, box)
		if err != nil {
			return err
		}
		boxCids = append(boxCids, cc)
	}

	for i, c := range boxCids {
		fname := fmt.Sprintf("split-%09d", i)

		contid, err := s.shuttleCreateContent(ctx, pin.UserID, c, fname, "", pin.Content)
		if err != nil {
			return err
		}

		cpin := &Pin{
			Cid:       util.DbCID{CID: c},
			Content:   contid,
			Active:    false,
			Pinning:   true,
			UserID:    pin.UserID,
			DagSplit:  true,
			SplitFrom: pin.Content,
		}

		if err := s.DB.Create(cpin).Error; err != nil {
			return xerrors.Errorf("failed to track new content in database: %w", err)
		}

		totalSize, objects, err := s.addDatabaseTrackingToContent(ctx, contid, dserv, s.Node.Blockstore, c, func(int64) {})
		if err != nil {
			return err
		}
		s.sendPinCompleteMessage(ctx, contid, totalSize, objects)
	}

	if err := s.DB.Model(Pin{}).Where("id = ?", pin.ID).UpdateColumns(map[string]interface{}{
		"dag_split": true,
		"size":      0,
		"active":    false,
		"pinning":   false,
	}).Error; err != nil {
		return err
	}

	if err := s.DB.Where("pin = ?", pin.ID).Delete(&ObjRef{}).Error; err != nil {
		return err
	}

	s.sendSplitContentComplete(ctx, pin.Content)
	return nil
}

func (s *Shuttle) handleRpcRestartTransfer(ctx context.Context, req *drpc.RestartTransfer) error {
	log.Debugf("restarting data transfer: %s", req.ChanID)
	st, err := s.Filc.TransferStatus(ctx, &req.ChanID)
	if err != nil && err != filclient.ErrNoTransferFound {
		return err
	}

	if st == nil {
		return fmt.Errorf("no transfer state was found for chanID: %s", req.ChanID)
	}

	cannotRestart := !util.CanRestartTransfer(st)
	if cannotRestart {
		if trsFailed, msg := util.TransferFailed(st); trsFailed {
			s.sendTransferStatusUpdate(ctx, &drpc.TransferStatus{
				DealDBID: req.DealDBID,
				Chanid:   req.ChanID.String(),
				State:    st,
				Failed:   true,
				Message:  fmt.Sprintf("status: %d(%s), message: %s", st.Status, msg, st.Message),
			})
			return fmt.Errorf("cannot restart transfer with status: %d", st.Status)
		}
		return nil
	}

	if err = s.Filc.RestartTransfer(ctx, &req.ChanID); err != nil {
		s.sendTransferStatusUpdate(ctx, &drpc.TransferStatus{
			DealDBID: req.DealDBID,
			Chanid:   req.ChanID.String(),
			State:    st,
			Failed:   true,
			Message:  fmt.Sprintf("failed to restart data transfer: %s", err),
		})
		return err
	}
	s.trackTransfer(&req.ChanID, req.DealDBID, st)
	return nil
}
