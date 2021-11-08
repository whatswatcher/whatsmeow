// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"encoding/binary"
	"fmt"
	"time"

	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/libsignal/groups"
	"go.mau.fi/libsignal/keys/prekey"
	"go.mau.fi/libsignal/protocol"
	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Number of sent messages to cache in memory for handling retry receipts.
const recentMessagesSize = 256

type recentMessageKey struct {
	To types.JID
	ID types.MessageID
}

// RecentMessage contains the info needed to re-send a message when another device fails to decrypt it.
type RecentMessage struct {
	Proto     *waProto.Message
	Timestamp time.Time
}

func (cli *Client) addRecentMessage(to types.JID, id types.MessageID, message *waProto.Message) {
	cli.recentMessagesLock.Lock()
	key := recentMessageKey{to, id}
	if cli.recentMessagesList[cli.recentMessagesPtr].ID != "" {
		delete(cli.recentMessagesMap, cli.recentMessagesList[cli.recentMessagesPtr])
	}
	cli.recentMessagesMap[key] = message
	cli.recentMessagesList[cli.recentMessagesPtr] = key
	cli.recentMessagesPtr++
	if cli.recentMessagesPtr >= len(cli.recentMessagesList) {
		cli.recentMessagesPtr = 0
	}
	fmt.Printf("Added %s/%s to message cache\n", to, id)
	cli.recentMessagesLock.Unlock()
}

func (cli *Client) getRecentMessage(to types.JID, id types.MessageID) *waProto.Message {
	cli.recentMessagesLock.RLock()
	msg, _ := cli.recentMessagesMap[recentMessageKey{to, id}]
	fmt.Printf("Message cache result for %s/%s: %+v\n", to, id, msg)
	cli.recentMessagesLock.RUnlock()
	return msg
}

func (cli *Client) getMessageForRetry(receipt *events.Receipt, messageID types.MessageID) (*waProto.Message, error) {
	msg := cli.getRecentMessage(receipt.Chat, messageID)
	if msg == nil {
		msg = cli.GetMessageForRetry(receipt.Chat, messageID)
		if msg == nil {
			return nil, fmt.Errorf("couldn't find message %s", messageID)
		} else {
			cli.Log.Debugf("Found message in GetMessageForRetry to accept retry receipt for %s/%s from %s", receipt.Chat, messageID, receipt.Sender)
		}
	} else {
		cli.Log.Debugf("Found message in local cache to accept retry receipt for %s/%s from %s", receipt.Chat, messageID, receipt.Sender)
	}
	return proto.Clone(msg).(*waProto.Message), nil
}

// handleRetryReceipt handles an incoming retry receipt for an outgoing message.
func (cli *Client) handleRetryReceipt(receipt *events.Receipt, node *waBinary.Node) error {
	retryChild, ok := node.GetOptionalChildByTag("retry")
	if !ok {
		return fmt.Errorf("missing <retry> element in retry receipt")
	}
	ag := retryChild.AttrGetter()
	messageID := ag.String("id")
	timestamp := time.Unix(ag.Int64("t"), 0)
	retryCount := ag.Int("count")
	if !ag.OK() {
		return ag.Error()
	}
	msg, err := cli.getMessageForRetry(receipt, messageID)
	if err != nil {
		return err
	}

	if receipt.IsGroup {
		builder := groups.NewGroupSessionBuilder(cli.Store, pbSerializer)
		senderKeyName := protocol.NewSenderKeyName(receipt.Chat.String(), cli.Store.ID.SignalAddress())
		signalSKDMessage, err := builder.Create(senderKeyName)
		if err != nil {
			cli.Log.Warnf("Failed to create sender key distribution message to include in retry of %s in %s to %s: %v", messageID, receipt.Chat, receipt.Sender, err)
		} else {
			msg.SenderKeyDistributionMessage = &waProto.SenderKeyDistributionMessage{
				GroupId:                             proto.String(receipt.Chat.String()),
				AxolotlSenderKeyDistributionMessage: signalSKDMessage.Serialize(),
			}
		}
	} else if receipt.IsFromMe {
		msg = &waProto.Message{
			DeviceSentMessage: &waProto.DeviceSentMessage{
				DestinationJid: proto.String(receipt.Chat.String()),
				Message:        msg,
			},
		}
	}
	plaintext, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	_, hasKeys := node.GetOptionalChildByTag("keys")
	var bundle *prekey.Bundle
	if hasKeys {
		bundle, err = nodeToPreKeyBundle(uint32(receipt.Sender.Device), *node)
		if err != nil {
			return fmt.Errorf("failed to read prekey bundle in retry receipt: %w", err)
		}
	} else if retryCount >= 2 {
		cli.Log.Debugf("Fetching prekeys for %s due to retry receipt with count>1 but no prekey bundle", receipt.Sender)
		var keys map[types.JID]preKeyResp
		keys, err = cli.fetchPreKeys([]types.JID{receipt.Sender})
		if err != nil {
			return err
		}
		bundle, err = keys[receipt.Sender].bundle, keys[receipt.Sender].err
		if err != nil {
			return fmt.Errorf("failed to fetch prekeys: %w", err)
		}
	}
	encrypted, err := cli.encryptMessageForDevice(plaintext, receipt.Sender, bundle)
	if err != nil {
		return fmt.Errorf("failed to encrypt message for retry: %w", err)
	}
	encrypted.Attrs["count"] = retryCount

	attrs := waBinary.Attrs{
		"to":   node.Attrs["from"],
		"type": "text",
		"id":   messageID,
		"t":    timestamp.Unix(),
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if edit, ok := node.Attrs["edit"]; ok {
		attrs["edit"] = edit
	}
	err = cli.sendNode(waBinary.Node{
		Tag:     "message",
		Attrs:   attrs,
		Content: []waBinary.Node{*encrypted},
	})
	if err != nil {
		return fmt.Errorf("failed to send retry message: %w", err)
	}
	cli.Log.Debugf("Sent retry #%d for %s/%s to %s", retryCount, receipt.Chat, messageID, receipt.Sender)
	return nil
}

// sendRetryReceipt sends a retry receipt for an incoming message.
func (cli *Client) sendRetryReceipt(node *waBinary.Node, forceIncludeIdentity bool) {
	id, _ := node.Attrs["id"].(string)
	children := node.GetChildren()
	var retryCountInMsg int
	if len(children) == 1 && children[0].Tag == "enc" {
		retryCountInMsg = children[0].AttrGetter().OptionalInt("count")
	}

	cli.messageRetriesLock.Lock()
	cli.messageRetries[id]++
	retryCount := cli.messageRetries[id]
	// In case the message is a retry response, and we restarted in between, find the count from the message
	if retryCount == 1 && retryCountInMsg > 0 {
		retryCount = retryCountInMsg + 1
		cli.messageRetries[id] = retryCount
	}
	cli.messageRetriesLock.Unlock()
	if retryCount >= 5 {
		cli.Log.Warnf("Not sending any more retry receipts for %s", id)
		return
	}

	var registrationIDBytes [4]byte
	binary.BigEndian.PutUint32(registrationIDBytes[:], cli.Store.RegistrationID)
	attrs := waBinary.Attrs{
		"id":   id,
		"type": "retry",
		"to":   node.Attrs["from"],
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	payload := waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
		Content: []waBinary.Node{
			{Tag: "retry", Attrs: waBinary.Attrs{
				"count": retryCount,
				"id":    id,
				"t":     node.Attrs["t"],
				"v":     1,
			}},
			{Tag: "registration", Content: registrationIDBytes[:]},
		},
	}
	if retryCount > 1 || forceIncludeIdentity {
		if key, err := cli.Store.PreKeys.GenOnePreKey(); err != nil {
			cli.Log.Errorf("Failed to get prekey for retry receipt: %v", err)
		} else if deviceIdentity, err := proto.Marshal(cli.Store.Account); err != nil {
			cli.Log.Errorf("Failed to marshal account info: %v", err)
			return
		} else {
			payload.Content = append(payload.GetChildren(), waBinary.Node{
				Tag: "keys",
				Content: []waBinary.Node{
					{Tag: "type", Content: []byte{ecc.DjbType}},
					{Tag: "identity", Content: cli.Store.IdentityKey.Pub[:]},
					preKeyToNode(key),
					preKeyToNode(cli.Store.SignedPreKey),
					{Tag: "device-identity", Content: deviceIdentity},
				},
			})
		}
	}
	err := cli.sendNode(payload)
	if err != nil {
		cli.Log.Errorf("Failed to send retry receipt for %s: %v", id, err)
	}
}
