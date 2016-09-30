// Copyright 2016 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package service

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/net/context"

	"github.com/keybase/client/go/engine"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
	keybase1 "github.com/keybase/client/go/protocol/keybase1"
	rpc "github.com/keybase/go-framed-msgpack-rpc"
)

// chatLocalHandler implements keybase1.chatLocal.
type chatLocalHandler struct {
	*BaseHandler
	libkb.Contextified
	gh    *gregorHandler
	boxer *chatBoxer

	// for test only
	rc chat1.RemoteInterface
}

// newChatLocalHandler creates a chatLocalHandler.
func newChatLocalHandler(xp rpc.Transporter, g *libkb.GlobalContext, gh *gregorHandler) *chatLocalHandler {
	return &chatLocalHandler{
		BaseHandler:  NewBaseHandler(xp),
		Contextified: libkb.NewContextified(g),
		gh:           gh,
		boxer:        newChatBoxer(g),
	}
}

// aggregateRateLimits takes a list of rate limit responses and dedups them to the last one received
// of each category
func (h *chatLocalHandler) aggRateLimits(rlimits []*chat1.RateLimit) (res []chat1.RateLimit) {
	m := make(map[string]chat1.RateLimit)
	for _, l := range rlimits {
		if l != nil {
			m[l.Name] = *l
		}
	}
	for _, v := range m {
		res = append(res, v)
	}
	return res
}

func (h *chatLocalHandler) tlfNameToTlfID(ctx context.Context, tlfName string) (tlfID chat1.TLFID, err error) {
	resp, err := h.boxer.tlf.CryptKeys(ctx, tlfName)
	if err != nil {
		return nil, err
	}
	tlfIDb := resp.TlfID.ToBytes()
	if tlfIDb == nil {
		return nil, errors.New("invalid TLF ID acquired")
	}
	tlfID = chat1.TLFID(tlfIDb)
	return tlfID, nil
}

func (h *chatLocalHandler) canonicalizeTlfName(ctx context.Context, tlfName string) (cName string, err error) {
	resp, err := h.boxer.tlf.CryptKeys(ctx, tlfName)
	if err != nil {
		return "", err
	}
	return string(resp.CanonicalName), nil
}

func (h *chatLocalHandler) getInboxQueryLocalToRemote(ctx context.Context, lquery *chat1.GetInboxLocalQuery) (rquery *chat1.GetInboxQuery, err error) {
	if lquery == nil {
		return nil, nil
	}
	rquery = &chat1.GetInboxQuery{}
	if lquery.TlfName != nil {
		tlfID, err := h.tlfNameToTlfID(ctx, *lquery.TlfName)
		if err != nil {
			return nil, err
		}
		rquery.TlfID = &tlfID
	}
	rquery.After = lquery.After
	rquery.Before = lquery.Before
	rquery.TlfVisibility = lquery.TlfVisibility
	rquery.TopicType = lquery.TopicType
	rquery.UnreadOnly = lquery.UnreadOnly
	rquery.ReadOnly = lquery.ReadOnly

	return rquery, nil
}

// GetInboxLocal implements keybase.chatLocal.getInboxLocal protocol.
func (h *chatLocalHandler) GetInboxLocal(ctx context.Context, arg chat1.GetInboxLocalArg) (inbox chat1.GetInboxLocalRes, err error) {
	if err := h.assertLoggedIn(ctx); err != nil {
		return chat1.GetInboxLocalRes{}, err
	}

	rquery, err := h.getInboxQueryLocalToRemote(ctx, arg.Query)
	if err != nil {
		return chat1.GetInboxLocalRes{}, err
	}
	ib, err := h.remoteClient().GetInboxRemote(ctx, chat1.GetInboxRemoteArg{
		Query:      rquery,
		Pagination: arg.Pagination,
	})
	if err != nil {
		return chat1.GetInboxLocalRes{}, err
	}
	inbox = chat1.GetInboxLocalRes{
		Pagination: arg.Pagination,
		RateLimits: h.aggRateLimits([]*chat1.RateLimit{ib.RateLimit}),
	}

	for _, convRemote := range ib.Inbox.Conversations {
		convLocal, err := h.localizeConversation(ctx, convRemote)
		if err != nil {
			return chat1.GetInboxLocalRes{}, err
		}

		if rquery.TlfID != nil {
			// verify using signed TlfName to make sure server returned genuine conversation
			signedTlfID, err := h.tlfNameToTlfID(ctx, convLocal.Info.TlfName)
			if err != nil {
				return chat1.GetInboxLocalRes{}, err
			}
			if !signedTlfID.Eq(*rquery.TlfID) {
				return chat1.GetInboxLocalRes{}, errors.New("server returned conversations for different TLF than query")
			}
		}

		// server can't query on topic name, so we'd have to do it ourselves in the loop
		if arg.Query.TopicName != nil && *arg.Query.TopicName != convLocal.Info.TopicName {
			continue
		}

		inbox.Conversations = append(inbox.Conversations, convLocal)
	}

	return inbox, nil
}

// GetThreadLocal implements keybase.chatLocal.getThreadLocal protocol.
func (h *chatLocalHandler) GetThreadLocal(ctx context.Context, arg chat1.GetThreadLocalArg) (chat1.GetThreadLocalRes, error) {
	if err := h.assertLoggedIn(ctx); err != nil {
		return chat1.GetThreadLocalRes{}, err
	}
	rarg := chat1.GetThreadRemoteArg{
		ConversationID: arg.ConversationID,
		Query:          arg.Query,
		Pagination:     arg.Pagination,
	}
	boxed, err := h.remoteClient().GetThreadRemote(ctx, rarg)
	if err != nil {
		return chat1.GetThreadLocalRes{}, err
	}

	thread, err := h.unboxThread(ctx, boxed.Thread, arg.ConversationID)
	if err != nil {
		return chat1.GetThreadLocalRes{}, err
	}

	return chat1.GetThreadLocalRes{
		Thread:     thread,
		RateLimits: h.aggRateLimits([]*chat1.RateLimit{boxed.RateLimit}),
	}, nil
}

// NewConversationLocal implements keybase.chatLocal.newConversationLocal protocol.
func (h *chatLocalHandler) NewConversationLocal(ctx context.Context, info chat1.ConversationInfoLocal) (fres chat1.NewConversationLocalRes, err error) {
	h.G().Log.Debug("NewConversationLocal: %+v", info)
	if err = h.assertLoggedIn(ctx); err != nil {
		return chat1.NewConversationLocalRes{}, err
	}
	res, err := h.boxer.tlf.CryptKeys(ctx, info.TlfName)
	if err != nil {
		return chat1.NewConversationLocalRes{}, fmt.Errorf("error getting crypt keys %s", err)
	}
	tlfIDb := res.TlfID.ToBytes()
	if tlfIDb == nil {
		return chat1.NewConversationLocalRes{}, errors.New("invalid TlfID acquired")
	}
	tlfID := chat1.TLFID(tlfIDb)

	info.Triple = chat1.ConversationIDTriple{
		Tlfid:     tlfID,
		TopicType: info.Triple.TopicType,
		TopicID:   make(chat1.TopicID, 16),
	}
	info.TlfName = string(res.CanonicalName)

	for i := 0; i < 3; i++ {
		if info.Triple.TopicType != chat1.TopicType_CHAT {
			// We only set topic ID if it's not CHAT. We are supporting only one
			// conversation per TLF now. A topic ID of 0s is intentional as it would
			// cause insertion failure in database.

			if info.Triple.TopicID, err = libkb.NewChatTopicID(); err != nil {
				return chat1.NewConversationLocalRes{}, fmt.Errorf("error creating topic ID: %s", err)
			}
		}

		firstMessageBoxed, err := h.prepareMessageForRemote(ctx, makeFirstMessage(ctx, info))
		if err != nil {
			return chat1.NewConversationLocalRes{}, fmt.Errorf("error preparing message: %s", err)
		}

		var res chat1.NewConversationRemoteRes
		res, err = h.remoteClient().NewConversationRemote2(ctx, chat1.NewConversationRemote2Arg{
			IdTriple:   info.Triple,
			TLFMessage: *firstMessageBoxed,
		})
		fres.RateLimits = h.aggRateLimits([]*chat1.RateLimit{res.RateLimit})
		if err != nil {
			if cerr, ok := err.(libkb.ChatConvExistsError); ok {
				if info.Triple.TopicType == chat1.TopicType_CHAT {
					// A chat conversation already exists; just reuse it.
					info.Id = cerr.ConvID
					fres.Conv = info
					return fres, nil
				}

				// Not a chat conversation. Multiples are fine. Just retry with a
				// different topic ID.
				continue
			}
		}

		info.Id = res.ConvID
		fres.Conv = info

		return fres, nil
	}

	return chat1.NewConversationLocalRes{}, err
}

// UpdateTopicNameLocal implements keybase.chatLocal.updateTopicNameLocal protocol.
func (h *chatLocalHandler) UpdateTopicNameLocal(ctx context.Context, arg chat1.UpdateTopicNameLocalArg) (chat1.UpdateTopicNameLocalRes, error) {
	var rlimits []*chat1.RateLimit
	if err := h.assertLoggedIn(ctx); err != nil {
		return chat1.UpdateTopicNameLocalRes{}, err
	}
	info, _, err := h.getConversationInfoByID(ctx, arg.ConversationID, &rlimits)
	if err != nil {
		return chat1.UpdateTopicNameLocalRes{}, err
	}
	plres, err := h.PostLocal(ctx, chat1.PostLocalArg{
		ConversationID:   info.Id,
		MessagePlaintext: makeUnboxedMessageToUpdateTopicName(ctx, info),
	})
	for _, rl := range plres.RateLimits {
		rlimits = append(rlimits, &rl)
	}
	if err != nil {
		return chat1.UpdateTopicNameLocalRes{}, err
	}
	return chat1.UpdateTopicNameLocalRes{
		RateLimits: h.aggRateLimits(rlimits),
	}, nil
}

func makeFirstMessage(ctx context.Context, conversationInfo chat1.ConversationInfoLocal) chat1.MessagePlaintext {
	if len(conversationInfo.TopicName) > 0 {
		return makeUnboxedMessageToUpdateTopicName(ctx, conversationInfo)
	}
	v1 := chat1.MessagePlaintextV1{
		ClientHeader: chat1.MessageClientHeader{
			Conv:        conversationInfo.Triple,
			TlfName:     conversationInfo.TlfName,
			TlfPublic:   conversationInfo.Visibility == chat1.TLFVisibility_PUBLIC,
			MessageType: chat1.MessageType_TLFNAME,
			Prev:        nil, // TODO
			// Sender and SenderDevice filled by PostLocal
		},
	}
	return chat1.NewMessagePlaintextWithV1(v1)
}

func makeUnboxedMessageToUpdateTopicName(ctx context.Context, conversationInfo chat1.ConversationInfoLocal) (messagePlaintext chat1.MessagePlaintext) {
	v1 := chat1.MessagePlaintextV1{
		ClientHeader: chat1.MessageClientHeader{
			Conv:        conversationInfo.Triple,
			TlfName:     conversationInfo.TlfName,
			TlfPublic:   conversationInfo.Visibility == chat1.TLFVisibility_PUBLIC,
			MessageType: chat1.MessageType_METADATA,
			Prev:        nil, // TODO
			// Sender and SenderDevice filled by PostLocal
		},
		MessageBody: chat1.NewMessageBodyWithMetadata(
			chat1.MessageConversationMetadata{
				ConversationTitle: conversationInfo.TopicName,
			}),
	}
	return chat1.NewMessagePlaintextWithV1(v1)
}

func (h *chatLocalHandler) GetInboxSummaryLocal(ctx context.Context, arg chat1.GetInboxSummaryLocalQuery) (res chat1.GetInboxSummaryLocalRes, err error) {
	if err = h.assertLoggedIn(ctx); err != nil {
		return chat1.GetInboxSummaryLocalRes{}, err
	}

	var after time.Time
	if len(arg.After) > 0 {
		after, err = parseTimeFromRFC3339OrDurationFromPast(arg.After)
		if err != nil {
			return chat1.GetInboxSummaryLocalRes{}, fmt.Errorf("parsing time or duration (%s) error: %s", arg.After, err)
		}
	}
	var before time.Time
	if len(arg.Before) > 0 {
		before, err = parseTimeFromRFC3339OrDurationFromPast(arg.Before)
		if err != nil {
			return chat1.GetInboxSummaryLocalRes{}, fmt.Errorf("parsing time or duration (%s) error: %s", arg.Before, err)
		}
	}

	var queryBase chat1.GetInboxQuery
	if !after.IsZero() {
		gafter := gregor1.ToTime(after)
		queryBase.After = &gafter
	}
	if !before.IsZero() {
		gbefore := gregor1.ToTime(before)
		queryBase.Before = &gbefore
	}
	if arg.TopicType != chat1.TopicType_NONE {
		queryBase.TopicType = &arg.TopicType
	}
	if arg.Visibility != chat1.TLFVisibility_ANY {
		queryBase.TlfVisibility = &arg.Visibility
	}

	fetchInbox := func(num int, query chat1.GetInboxQuery) (conversations []chat1.ConversationLocal, rl []chat1.RateLimit, err error) {
		var rpcArg chat1.GetInboxLocalArg
		rpcArg.Pagination = &chat1.Pagination{Num: num}
		rpcArg.Query = &query

		gilres, err := h.GetInboxLocal(ctx, rpcArg)
		if err != nil {
			return nil, nil, err
		}
		iview := gilres.Inbox
		for _, conv := range iview.Conversations {
			info, maxMessages, err := h.getConversationInfo(ctx, conv)
			if err != nil {
				return nil, nil, err
			}
			c := chat1.ConversationLocal{
				Info:     &info,
				Messages: maxMessages,
				ReadUpTo: conv.ReaderInfo.ReadMsgid,
			}
			conversations = append(conversations, c)
		}

		return conversations, gilres.RateLimits, nil
	}

	var convs []chat1.ConversationLocal

	if arg.UnreadFirst {
		if arg.UnreadFirstLimit.AtMost <= 0 {
			arg.UnreadFirstLimit.AtMost = int(^uint(0) >> 1) // maximum int
		}
		queryBase.UnreadOnly, queryBase.ReadOnly = true, false
		if convs, res.RateLimits, err = fetchInbox(arg.UnreadFirstLimit.AtMost, queryBase); err != nil {
			return chat1.GetInboxSummaryLocalRes{}, err
		}
		res.Conversations = append(res.Conversations, convs...)

		more := collar(
			arg.UnreadFirstLimit.AtLeast-len(res.Conversations),
			arg.UnreadFirstLimit.NumRead,
			arg.UnreadFirstLimit.AtMost-len(res.Conversations),
		)
		if more > 0 {
			queryBase.UnreadOnly, queryBase.ReadOnly = false, true
			if convs, res.RateLimits, err = fetchInbox(more, queryBase); err != nil {
				return chat1.GetInboxSummaryLocalRes{}, err
			}
			res.Conversations = append(res.Conversations, convs...)
		}
	} else {
		if arg.ActivitySortedLimit <= 0 {
			arg.ActivitySortedLimit = int(^uint(0) >> 1) // maximum int
		}
		queryBase.UnreadOnly, queryBase.ReadOnly = false, false
		if convs, res.RateLimits, err = fetchInbox(arg.ActivitySortedLimit, queryBase); err != nil {
			return chat1.GetInboxSummaryLocalRes{}, err
		}
		res.Conversations = append(res.Conversations, convs...)
	}

	res.MoreTotal = 1000 // TODO: implement this on server

	return res, nil
}

func (h *chatLocalHandler) localizeConversation(
	ctx context.Context, conversationRemote chat1.Conversation) (
	conversationLocal chat1.ConversationLocal, err error) {

	conversationLocal.Info = &ConversationInfoLocal{
		Id: conversationRemote.Metadata.ConversationID,
	}

	if len(conversationRemote.MaxMsgs) == 0 {
		return chat1.ConversationLocal{},
			libkb.UnexpectedChatDataFromServer{Msg: "conversation has an empty MaxMsgs field"}
	}
	if conversationLocal.MaxMessages, err = h.unboxMessages(convRemote.MaxMsgs); err != nil {
		return chat1.ConversationLocal{}, err
	}

	if conversationRemote.ReaderInfo == nil {
		return chat1.ConversationLocal{},
			libkb.UnexpectedChatDataFromServer{Msg: "empty ReaderInfo from server?"}
	}
	conversationLocal.ReaderInfo = *convRemote.ReaderInfo

	var maxValidID chat1.MessageID
	for _, messagePlaintext := range conversationLocal.MaxMessages {
		version, err := messagePlaintext.Version()
		if err != nil {
			return chat1.ConversationLocal{}, err
		}
		switch version {
		case chat1.MessagePlaintextVersion_V1:
			body := messagePlaintext.V1().MessageBody

			if t, err := body.MessageType(); err != nil {
				return chat1.ConversationLocal{}, err
			} else if t == chat1.MessageType_METADATA {
				conversationLocal.Info.TopicName = body.Metadata().ConversationTitle
			}

			if b.ServerHeader.MessageID >= maxValidID {
				conversationLocal.Info.TlfName = messagePlaintext.V1().ClientHeader.TlfName
				maxValidID = b.ServerHeader.MessageID
			}
			conversationLocal.Info.Triple = messagePlaintext.V1().ClientHeader.Conv
		default:
			return chat1.ConversationLocal{}, libkb.NewChatMessageVersionError(version)
		}
	}

	if len(conversationInfo.TlfName) == 0 {
		return chat1.ConversationLocal{}, errors.New("no valid message in the conversation")
	}

	if conversationInfo.TlfName, err = h.canonicalizeTlfName(ctx, conversationInfo.TlfName); err != nil {
		return chat1.ConversationLocal{}, err
	}

	// verify Conv matches ConversationIDTriple in MessageClientHeader
	if !conversationRemote.Metadata.IdTriple.Eq(conversationInfo.Triple) {
		return chat1.ConversationLocal{}, errors.New("server header conversation triple does not match client header triple")
	}

	return conversationInfo, maxMessages, nil
}

func (h *chatLocalHandler) getSenderInfoLocal(uimap *userInfoMapper, messagePlaintext chat1.MessagePlaintext) (senderUsername string, senderDeviceName string, err error) {
	version, err := messagePlaintext.Version()
	if err != nil {
		return "", "", err
	}
	switch version {
	case chat1.MessagePlaintextVersion_V1:
		v1 := messagePlaintext.V1()
		uid := keybase1.UID(v1.ClientHeader.Sender.String())
		did := keybase1.DeviceID(v1.ClientHeader.SenderDevice.String())
		username, deviceName, err := uimap.lookup(uid, did)
		if err != nil {
			return "", "", err
		}

		return username, deviceName, nil

	default:
		return "", "", libkb.NewChatMessageVersionError(version)
	}
}

// GetMessagesLocal implements keybase.chatLocal.GetMessagesLocal protocol.
func (h *chatLocalHandler) GetConversationForCLILocal(ctx context.Context, arg chat1.GetConversationForCLILocalQuery) (res chat1.GetConversationForCLILocalRes, err error) {
	if err := h.assertLoggedIn(ctx); err != nil {
		return chat1.GetMessagesLocalRes{}, err
	}

	var rlimits []*chat1.RateLimit

	if arg.Limit.AtMost <= 0 {
		arg.Limit.AtMost = int(^uint(0) >> 1) // maximum int
	}

	ibres, err := h.remoteClient().GetInboxLocal(ctx, chat1.GetInboxLocalArg{
		Query: &chat1.GetInboxQuery{
			ConvID: &cid,
		},
	})
	if err != nil {
		return chat1.GetMessagesLocalRes{}, fmt.Errorf("getting conversation %v error: %v", cid, err)
	}
	rlimits = append(rlimits, ibres.RateLimit)
	if len(ibres.Conversations) == 0 {
		return chat1.GetMessagesLocalRes{}, fmt.Errorf("unknown conversation: %v", cid)
	}

	var since time.Time
	if selector.Since != nil {
		since, err = parseTimeFromRFC3339OrDurationFromPast(*selector.Since)
		if err != nil {
			return chat1.ConversationLocal{}, fmt.Errorf("parsing time or duration (%s) error: %s", *selector.Since, since)
		}
	}

	query := chat1.GetThreadQuery{
		MarkAsRead:   arg.MarkAsRead,
		MessageTypes: arg.MessageTypes,
	}
	if !since.IsZero() {
		gsince := gregor1.ToTime(since)
		query.After = &gsince
	}

	tv, err := h.GetThreadLocal(ctx, chat1.GetThreadLocalArg{
		ConversationID: arg.ConversationId,
		Query:          query,
	})

	if err != nil {
		return chat1.ConversationLocal{}, err
	}
	for _, rl := range tview.RateLimits {
		*rlimits = append(*rlimits, &rl)
	}

	var messages []chat1.MessageFromServerOrError
	for _, m := range tview.Thread.Messages {
		if selector.OnlyNew &&
			conversationRemote.ReaderInfo != nil && m.Message != nil &&
			m.Message.ServerHeader.MessageID <= conversationRemote.ReaderInfo.ReadMsgid {
			break
		}

		messages = append(messages, m)

		selector.Limit.AtMost--
		selector.Limit.AtLeast--
		if m.Message.ServerHeader.MessageID <= conversationRemote.ReaderInfo.ReadMsgid {
			selector.Limit.NumRead--
		}
		if selector.Limit.AtMost <= 0 ||
			(selector.Limit.NumRead <= 0 && selector.Limit.AtLeast <= 0) {
			break
		}
	}

	return chat1.GetConversationForCLILocalRes{
		Conversation: ibres.Conversations[0],
		Messages:     messages,
		RateLimits:   h.aggRateLimits(rlimits),
	}, nil
}

func (h *chatLocalHandler) addSenderToMessage(msg chat1.MessagePlaintext) (chat1.MessagePlaintext, error) {
	uid := h.G().Env.GetUID()
	if uid.IsNil() {
		return chat1.MessagePlaintext{}, libkb.LoginRequiredError{}
	}
	did := h.G().Env.GetDeviceID()
	if did.IsNil() {
		return chat1.MessagePlaintext{}, libkb.DeviceRequiredError{}
	}

	huid := uid.ToBytes()
	if huid == nil {
		return chat1.MessagePlaintext{}, errors.New("invalid UID")
	}

	hdid := make([]byte, libkb.DeviceIDLen)
	if err := did.ToBytes(hdid); err != nil {
		return chat1.MessagePlaintext{}, err
	}

	version, err := msg.Version()
	if err != nil {
		return chat1.MessagePlaintext{}, err
	}

	switch version {
	case chat1.MessagePlaintextVersion_V1:
		header := msg.V1().ClientHeader
		header.Sender = gregor1.UID(huid)
		header.SenderDevice = gregor1.DeviceID(hdid)
		updated := chat1.MessagePlaintextV1{
			ClientHeader: header,
			MessageBody:  msg.V1().MessageBody,
		}
		return chat1.NewMessagePlaintextWithV1(updated), nil
	default:
		return chat1.MessagePlaintext{}, libkb.NewChatMessageVersionError(version)
	}

}

func (h *chatLocalHandler) prepareMessageForRemote(ctx context.Context, plaintext chat1.MessagePlaintext) (*chat1.MessageBoxed, error) {
	msg, err := h.addSenderToMessage(plaintext)
	if err != nil {
		return nil, err
	}

	// encrypt the message
	skp, err := h.getSigningKeyPair()
	if err != nil {
		return nil, err
	}
	boxed, err := h.boxer.boxMessage(ctx, msg, skp)
	if err != nil {
		return nil, err
	}

	// TODO: populate plaintext.ClientHeader.Conv

	return boxed, nil
}

// PostLocal implements keybase.chatLocal.postLocal protocol.
func (h *chatLocalHandler) PostLocal(ctx context.Context, arg chat1.PostLocalArg) (chat1.PostLocalRes, error) {
	if err := h.assertLoggedIn(ctx); err != nil {
		return chat1.PostLocalRes{}, err
	}
	boxed, err := h.prepareMessageForRemote(ctx, arg.MessagePlaintext)
	if err != nil {
		return chat1.PostLocalRes{}, err
	}

	// post to remote gregord
	rarg := chat1.PostRemoteArg{
		ConversationID: arg.ConversationID,
		MessageBoxed:   *boxed,
	}

	plres, err := h.remoteClient().PostRemote(ctx, rarg)
	if err != nil {
		return chat1.PostLocalRes{}, err
	}
	return chat1.PostLocalRes{
		RateLimits: h.aggRateLimits([]*chat1.RateLimit{plres.RateLimit}),
	}, nil
}

func (h *chatLocalHandler) getSigningKeyPair() (kp libkb.NaclSigningKeyPair, err error) {
	// get device signing key for this user
	signingKey, err := engine.GetMySecretKey(h.G(), h.getSecretUI, libkb.DeviceSigningKeyType, "sign chat message")
	if err != nil {
		return libkb.NaclSigningKeyPair{}, err
	}
	kp, ok := signingKey.(libkb.NaclSigningKeyPair)
	if !ok || kp.Private == nil {
		return libkb.NaclSigningKeyPair{}, libkb.KeyCannotSignError{}
	}

	return kp, nil
}

// getSecretUI returns a SecretUI, preferring a delegated SecretUI if
// possible.
func (h *chatLocalHandler) getSecretUI() libkb.SecretUI {
	ui, err := h.G().UIRouter.GetSecretUI(0)
	if err == nil && ui != nil {
		h.G().Log.Debug("chatLocalHandler: using delegated SecretUI")
		return ui
	}
	h.G().Log.Debug("chatLocalHandler: using local SecretUI")
	return h.BaseHandler.getSecretUI(0, h.G())
}

// remoteClient returns a client connection to gregord.
func (h *chatLocalHandler) remoteClient() chat1.RemoteInterface {
	if h.rc != nil {
		return h.rc
	}
	return &chat1.RemoteClient{Cli: h.gh.cli}
}

// unboxThread transforms a chat1.ThreadViewBoxed to a keybase1.ThreadView.
func (h *chatLocalHandler) unboxThread(ctx context.Context, boxed chat1.ThreadViewBoxed, convID chat1.ConversationID) (chat1.ThreadView, error) {
	thread := chat1.ThreadView{
		Pagination: boxed.Pagination,
	}

	if thread.Messages, err = h.unboxMessages(boxed.Messages); err != nil {
		return chat1.ThreadView{}, err
	}

	return thread, nil
}

func (h *chatLocalHandler) unboxMessages(boxed []chat1.MessageBoxed) (unboxed []chat1.MessageFromServerOrError, err error) {
	finder := newKeyFinder()
	var uimap *userInfoMapper
	ctx, uimap = getUserInfoMapper(ctx, h.G())
	for _, msg := range boxed.Messages {
		messagePlaintext, err := h.boxer.unboxMessage(ctx, finder, msg)
		if err != nil {
			errMsg := err.Error()
			unboxed = append(unboxed, chat1.MessageFromServerOrError{
				UnboxingError: &errMsg,
			})
			continue
		}

		username, deviceName, err := h.getSenderInfoLocal(uimap, messagePlaintext)
		if err != nil {
			return nil, err
		}

		unboxed = append(unboxed, chat1.MessageFromServerOrError{
			Message: &chat1.MessageFromServer{
				SenderUsername:   username,
				SenderDeviceName: deviceName,
				ServerHeader:     *msg.ServerHeader,
				MessagePlaintext: messagePlaintext,
			},
		})
	}

	return unboxed, nil
}

func (h *chatLocalHandler) assertLoggedIn(ctx context.Context) error {
	ok, err := h.G().LoginState().LoggedInProvisionedLoad()
	if err != nil {
		return err
	}
	if !ok {
		return libkb.LoginRequiredError{}
	}
	return nil
}

// keyFinder remembers results from previous calls to CryptKeys().
// It is not intended to be used by multiple concurrent goroutines
// or held onto for very long, just to remember the keys while
// unboxing a thread of messages.
type keyFinder struct {
	keys map[string]keybase1.TLFCryptKeys
}

// newKeyFinder creates a keyFinder.
func newKeyFinder() *keyFinder {
	return &keyFinder{keys: make(map[string]keybase1.TLFCryptKeys)}
}

func (k *keyFinder) cacheKey(tlfName string, tlfPublic bool) string {
	return fmt.Sprintf("%s|%v", tlfName, tlfPublic)
}

// find finds keybase1.TLFCryptKeys for tlfName, checking for existing
// results.
func (k *keyFinder) find(ctx context.Context, tlf keybase1.TlfInterface, tlfName string, tlfPublic bool) (keybase1.TLFCryptKeys, error) {
	ckey := k.cacheKey(tlfName, tlfPublic)
	existing, ok := k.keys[ckey]
	if ok {
		return existing, nil
	}

	var keys keybase1.TLFCryptKeys
	if tlfPublic {
		cid, err := tlf.PublicCanonicalTLFNameAndID(ctx, tlfName)
		if err != nil {
			return keybase1.TLFCryptKeys{}, err
		}
		keys.CanonicalName = cid.CanonicalName
		keys.TlfID = cid.TlfID
		keys.CryptKeys = []keybase1.CryptKey{publicCryptKey}
	} else {
		var err error
		keys, err = tlf.CryptKeys(ctx, tlfName)
		if err != nil {
			return keybase1.TLFCryptKeys{}, err
		}
	}

	k.keys[ckey] = keys

	return keys, nil
}
