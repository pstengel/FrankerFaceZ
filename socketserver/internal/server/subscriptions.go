package server

// This is the scariest code I've written yet for the server.
// If I screwed up the locking, I won't know until it's too late.

import (
	"log"
	"sync"
	"time"
)

type SubscriberList struct {
	sync.RWMutex
	Members []chan<- ClientMessage
}

var ChatSubscriptionInfo map[string]*SubscriberList = make(map[string]*SubscriberList)
var ChatSubscriptionLock sync.RWMutex
var GlobalSubscriptionInfo SubscriberList

func SubscribeChannel(client *ClientInfo, channelName string) {
	ChatSubscriptionLock.RLock()
	_subscribeWhileRlocked(channelName, client.MessageChannel)
	ChatSubscriptionLock.RUnlock()
}

func SubscribeDefaults(client *ClientInfo) {

}

func SubscribeGlobal(client *ClientInfo) {
	GlobalSubscriptionInfo.Lock()
	AddToSliceC(&GlobalSubscriptionInfo.Members, client.MessageChannel)
	GlobalSubscriptionInfo.Unlock()
}

func PublishToChannel(channel string, msg ClientMessage) (count int) {
	ChatSubscriptionLock.RLock()
	list := ChatSubscriptionInfo[channel]
	if list != nil {
		list.RLock()
		for _, msgChan := range list.Members {
			msgChan <- msg
			count++
		}
		list.RUnlock()
	}
	ChatSubscriptionLock.RUnlock()
	return
}

func PublishToMultiple(channels []string, msg ClientMessage) (count int) {
	found := make(map[chan<- ClientMessage]struct{})

	ChatSubscriptionLock.RLock()

	for _, channel := range channels {
		list := ChatSubscriptionInfo[channel]
		if list != nil {
			list.RLock()
			for _, msgChan := range list.Members {
				found[msgChan] = struct{}{}
			}
			list.RUnlock()
		}
	}

	ChatSubscriptionLock.RUnlock()

	for msgChan, _ := range found {
		msgChan <- msg
		count++
	}
	return
}

func PublishToAll(msg ClientMessage) (count int) {
	GlobalSubscriptionInfo.RLock()
	for _, msgChan := range GlobalSubscriptionInfo.Members {
		msgChan <- msg
		count++
	}
	GlobalSubscriptionInfo.RUnlock()
	return
}

func UnsubscribeSingleChat(client *ClientInfo, channelName string) {
	ChatSubscriptionLock.RLock()
	list := ChatSubscriptionInfo[channelName]
	if list != nil {
		list.Lock()
		RemoveFromSliceC(&list.Members, client.MessageChannel)
		list.Unlock()
	}
	ChatSubscriptionLock.RUnlock()
}

// Unsubscribe the client from all channels, AND clear the CurrentChannels / WatchingChannels fields.
// Locks:
//   - read lock to top-level maps
//   - write lock to SubscriptionInfos
//   - write lock to ClientInfo
func UnsubscribeAll(client *ClientInfo) {
	client.Mutex.Lock()
	client.PendingSubscriptionsBacklog = nil
	client.PendingSubscriptionsBacklog = nil
	client.Mutex.Unlock()

	GlobalSubscriptionInfo.Lock()
	RemoveFromSliceC(&GlobalSubscriptionInfo.Members, client.MessageChannel)
	GlobalSubscriptionInfo.Unlock()

	ChatSubscriptionLock.RLock()
	client.Mutex.Lock()
	for _, v := range client.CurrentChannels {
		list := ChatSubscriptionInfo[v]
		if list != nil {
			list.Lock()
			RemoveFromSliceC(&list.Members, client.MessageChannel)
			list.Unlock()
		}
	}
	client.CurrentChannels = nil
	client.Mutex.Unlock()
	ChatSubscriptionLock.RUnlock()
}

func unsubscribeAllClients() {
	GlobalSubscriptionInfo.Lock()
	GlobalSubscriptionInfo.Members = nil
	GlobalSubscriptionInfo.Unlock()
	ChatSubscriptionLock.Lock()
	ChatSubscriptionInfo = make(map[string]*SubscriberList)
	ChatSubscriptionLock.Unlock()
}

const ReapingDelay = 1 * time.Minute

// Checks ChatSubscriptionInfo for entries with no subscribers every ReapingDelay.
// Started from SetupServer().
func pubsubJanitor() {
	for {
		time.Sleep(ReapingDelay)
		var cleanedUp = make([]string, 0, 6)
		ChatSubscriptionLock.Lock()
		for key, val := range ChatSubscriptionInfo {
			if val == nil || len(val.Members) == 0 {
				delete(ChatSubscriptionInfo, key)
				cleanedUp = append(cleanedUp, key)
			}
		}
		ChatSubscriptionLock.Unlock()

		if len(cleanedUp) != 0 {
			err := SendCleanupTopicsNotice(cleanedUp)
			if err != nil {
				log.Println("error reporting cleaned subs:", err)
			}
		}
	}
}

// Add a channel to the subscriptions while holding a read-lock to the map.
// Locks:
//   - ALREADY HOLDING a read-lock to the 'which' top-level map via the rlocker object
//   - possible write lock to the 'which' top-level map via the wlocker object
//   - write lock to SubscriptionInfo (if not creating new)
func _subscribeWhileRlocked(channelName string, value chan<- ClientMessage) {
	list := ChatSubscriptionInfo[channelName]
	if list == nil {
		// Not found, so create it
		ChatSubscriptionLock.RUnlock()
		ChatSubscriptionLock.Lock()
		list = &SubscriberList{}
		list.Members = []chan<- ClientMessage{value} // Create it populated, to avoid reaper
		ChatSubscriptionInfo[channelName] = list
		ChatSubscriptionLock.Unlock()

		go func(topic string) {
			err := SendNewTopicNotice(topic)
			if err != nil {
				log.Println("error reporting new sub:", err)
			}
		}(channelName)

		ChatSubscriptionLock.RLock()
	} else {
		list.Lock()
		AddToSliceC(&list.Members, value)
		list.Unlock()
	}
}
