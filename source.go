package main

import (
	"context"
	"sort"
	"time"

	"github.com/gotd/td/tg"
)

// pageSize is the number of messages fetched per API call.
const pageSize = 100

// video is a downloadable video message.
type video struct {
	MessageID int
	Date      time.Time
	Doc       *tg.Document
	Duration  int
	Width     int
	Height    int
	FileName  string
}

// Size returns the declared document size in bytes.
func (v video) Size() int64 { return v.Doc.Size }

// extractVideo returns the video carried by a message.
//
// Classification follows the official clients: a document is a video when it
// carries a DocumentAttributeVideo. The mime type is deliberately NOT checked,
// because videos posted "as file" are frequently tagged
// application/octet-stream while still carrying the correct attribute.
//
// Excluded:
//   - round video notes (selfie-style "video messages"),
//   - animations (GIF-style mp4), which carry DocumentAttributeAnimated.
func extractVideo(mc tg.MessageClass) (video, bool) {
	msg, ok := mc.(*tg.Message)
	if !ok || msg.Media == nil {
		return video{}, false
	}

	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return video{}, false
	}
	doc, ok := media.Document.AsNotEmpty()
	if !ok || doc == nil {
		return video{}, false
	}

	v := video{
		MessageID: msg.ID,
		Date:      time.Unix(int64(msg.Date), 0).UTC(),
		Doc:       doc,
	}

	var hasVideo, animated bool
	for _, raw := range doc.Attributes {
		switch a := raw.(type) {
		case *tg.DocumentAttributeVideo:
			if a.RoundMessage {
				// Video note, not a channel video.
				return video{}, false
			}
			hasVideo = true
			v.Duration = int(a.Duration)
			v.Width = a.W
			v.Height = a.H
		case *tg.DocumentAttributeAnimated:
			animated = true
		case *tg.DocumentAttributeFilename:
			v.FileName = a.FileName
		}
	}
	if !hasVideo || animated {
		return video{}, false
	}
	return v, true
}

// fetchPage requests one page of channel history.
//
// Telegram returns history newest first. The request yields messages with
// minID < id < offsetID; an offsetID of 0 means "start from the newest".
func fetchPage(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, minID, offsetID int) ([]tg.MessageClass, error) {
	res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     peer,
		OffsetID: offsetID,
		MinID:    minID,
		Limit:    pageSize,
	})
	if err != nil {
		return nil, err
	}
	switch m := res.(type) {
	case *tg.MessagesChannelMessages:
		return m.Messages, nil
	case *tg.MessagesMessages:
		return m.Messages, nil
	case *tg.MessagesMessagesSlice:
		return m.Messages, nil
	default:
		// messagesNotModified and empty responses both mean "nothing new".
		return nil, nil
	}
}

// historyPage is an ascending batch of messages plus the pagination cursor.
type historyPage struct {
	// Messages are sorted oldest first.
	Messages []tg.MessageClass
	// Lowest is the smallest message id on the page (0 when empty).
	Lowest int
	// Highest is the largest message id on the page (0 when empty).
	Highest int
	// Short is true when the server had fewer messages than pageSize, i.e.
	// this page reached the boundary of the requested range.
	Short bool
}

// nextPage returns the next ascending block of messages in (minID, offsetID).
//
// Telegram paginates descending, so the block is reversed here. Processing
// oldest first keeps a single monotonic cursor sufficient for crash recovery.
func nextPage(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, minID, offsetID int) (historyPage, error) {
	desc, err := fetchPage(ctx, api, peer, minID, offsetID)
	if err != nil {
		return historyPage{}, err
	}
	if len(desc) == 0 {
		return historyPage{Short: true}, nil
	}

	page := historyPage{
		Messages: make([]tg.MessageClass, 0, len(desc)),
		Short:    len(desc) < pageSize,
	}
	for _, mc := range desc {
		msg, ok := mc.(*tg.Message)
		if !ok {
			continue
		}
		page.Messages = append(page.Messages, mc)
		if page.Lowest == 0 || msg.ID < page.Lowest {
			page.Lowest = msg.ID
		}
		if msg.ID > page.Highest {
			page.Highest = msg.ID
		}
	}
	sort.SliceStable(page.Messages, func(i, j int) bool {
		return page.Messages[i].(*tg.Message).ID < page.Messages[j].(*tg.Message).ID
	})
	return page, nil
}
