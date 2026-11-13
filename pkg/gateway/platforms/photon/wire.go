package photon

import (
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Appends protobuf fields in the order the request messages declare them
type pbWriter struct{ b []byte }

func (w *pbWriter) str(num protowire.Number, s string) {
	if s == "" {
		return
	}
	w.b = protowire.AppendTag(w.b, num, protowire.BytesType)
	w.b = protowire.AppendString(w.b, s)
}

func (w *pbWriter) strAlways(num protowire.Number, s string) {
	w.b = protowire.AppendTag(w.b, num, protowire.BytesType)
	w.b = protowire.AppendString(w.b, s)
}

func (w *pbWriter) bytes(num protowire.Number, b []byte) {
	w.b = protowire.AppendTag(w.b, num, protowire.BytesType)
	w.b = protowire.AppendBytes(w.b, b)
}

func (w *pbWriter) msg(num protowire.Number, sub *pbWriter) {
	w.b = protowire.AppendTag(w.b, num, protowire.BytesType)
	w.b = protowire.AppendBytes(w.b, sub.b)
}

func (w *pbWriter) flag(num protowire.Number, v bool) {
	if !v {
		return
	}
	w.b = protowire.AppendTag(w.b, num, protowire.VarintType)
	w.b = protowire.AppendVarint(w.b, 1)
}

func (w *pbWriter) varint(num protowire.Number, v uint64) {
	if v == 0 {
		return
	}
	w.b = protowire.AppendTag(w.b, num, protowire.VarintType)
	w.b = protowire.AppendVarint(w.b, v)
}

// Writes a proto3 optional int32 whenever it is present, zero included
func (w *pbWriter) optInt32(num protowire.Number, v *int32) {
	if v == nil {
		return
	}
	w.b = protowire.AppendTag(w.b, num, protowire.VarintType)
	w.b = protowire.AppendVarint(w.b, uint64(int64(*v)))
}

func (w *pbWriter) optUint64(num protowire.Number, v *uint64) {
	if v == nil {
		return
	}
	w.b = protowire.AppendTag(w.b, num, protowire.VarintType)
	w.b = protowire.AppendVarint(w.b, *v)
}

// One decoded field, the varint or fixed value in val and the length delimited payload in buf
type pbField struct {
	num protowire.Number
	typ protowire.Type
	val uint64
	buf []byte
}

func (f pbField) str() string    { return string(f.buf) }
func (f pbField) boolean() bool  { return f.typ == protowire.VarintType && f.val != 0 }
func (f pbField) int32() int32   { return int32(f.val) }
func (f pbField) int64() int64   { return int64(f.val) }
func (f pbField) isBytes() bool  { return f.typ == protowire.BytesType }
func (f pbField) isVarint() bool { return f.typ == protowire.VarintType }

// Walks every field of a protobuf message, skipping groups and failing on truncated input
func pbFields(b []byte, fn func(f pbField) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		f := pbField{num: num, typ: typ}
		switch typ {
		case protowire.VarintType:
			f.val, n = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			f.val, n = protowire.ConsumeFixed64(b)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(b)
			f.val = uint64(v)
		case protowire.BytesType:
			f.buf, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

// Decoded google.protobuf.Timestamp, the zero time when absent
func decodeTimestamp(b []byte) (time.Time, error) {
	var sec, nanos int64
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			sec = f.int64()
		case 2:
			nanos = int64(f.int32())
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, nanos).UTC(), nil
}

// Chat service of an address as the SDK names it
var chatServiceNames = map[int32]string{1: "iMessage", 2: "SMS", 3: "RCS"}

type imAddress struct {
	Address string
	Country string
	Service string
}

func decodeAddress(b []byte) (*imAddress, error) {
	a := &imAddress{Service: "unknown"}
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			a.Address = f.str()
		case 2:
			if name, ok := chatServiceNames[f.int32()]; ok {
				a.Service = name
			}
		case 3:
			a.Country = f.str()
		}
		return nil
	})
	return a, err
}

type imAttachment struct {
	GUID       string
	FileName   string
	MimeType   string
	UTI        string
	TotalBytes int64
	IsHidden   bool
	IsSticker  bool
}

func decodeAttachment(b []byte) (imAttachment, error) {
	var a imAttachment
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			a.GUID = f.str()
		case 3:
			a.FileName = f.str()
		case 4:
			a.MimeType = f.str()
		case 5:
			a.UTI = f.str()
		case 6:
			a.TotalBytes = f.int64()
		case 9:
			a.IsHidden = f.boolean()
		case 10:
			a.IsSticker = f.boolean()
		}
		return nil
	})
	return a, err
}

// Tapback families of MessageReactionKind in wire order
var reactionKinds = []string{"love", "like", "dislike", "laugh", "emphasize", "question", "emoji", "sticker"}

type imReaction struct {
	Kind  string
	Emoji string
}

func decodeReaction(b []byte) (imReaction, error) {
	r := imReaction{Kind: "love"}
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			if k := f.int32(); k >= 0 && int(k) < len(reactionKinds) {
				r.Kind = reactionKinds[k]
			} else {
				r.Kind = "unknown"
			}
		case 2:
			r.Emoji = f.str()
		}
		return nil
	})
	return r, err
}

// Wire value of a settable reaction kind, false for kinds SetReaction refuses
func reactionKindValue(kind string) (uint64, bool) {
	for i, k := range reactionKinds[:7] {
		if k == kind {
			return uint64(i), true
		}
	}
	return 0, false
}

type imAppliedReaction struct {
	PartIndex *int32
	Reaction  imReaction
	IsFromMe  bool
}

func decodeAppliedReaction(b []byte) (imAppliedReaction, error) {
	var r imAppliedReaction
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 2:
			v := f.int32()
			r.PartIndex = &v
		case 3:
			rc, err := decodeReaction(f.buf)
			if err != nil {
				return err
			}
			r.Reaction = rc
		case 5:
			r.IsFromMe = f.boolean()
		}
		return nil
	})
	return r, err
}

type imMiniAppLayout struct {
	Caption            string
	Subcaption         string
	TrailingCaption    string
	TrailingSubcaption string
	Summary            string
}

type imMiniApp struct {
	AppName string
	URL     string
	Layout  *imMiniAppLayout
}

func decodeMiniApp(b []byte) (*imMiniApp, error) {
	m := &imMiniApp{}
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 3:
			m.AppName = f.str()
		case 4:
			m.URL = f.str()
		case 8:
			l := &imMiniAppLayout{}
			if err := pbFields(f.buf, func(lf pbField) error {
				switch lf.num {
				case 1:
					l.Caption = lf.str()
				case 2:
					l.Subcaption = lf.str()
				case 3:
					l.TrailingCaption = lf.str()
				case 4:
					l.TrailingSubcaption = lf.str()
				case 7:
					l.Summary = lf.str()
				}
				return nil
			}); err != nil {
				return err
			}
			m.Layout = l
		}
		return nil
	})
	return m, err
}

type imContent struct {
	Text        string
	Attachments []imAttachment
	MiniApp     *imMiniApp
}

func decodeContent(b []byte) (imContent, error) {
	var c imContent
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			c.Text = f.str()
		case 2:
			a, err := decodeAttachment(f.buf)
			if err != nil {
				return err
			}
			c.Attachments = append(c.Attachments, a)
		case 7:
			m, err := decodeMiniApp(f.buf)
			if err != nil {
				return err
			}
			c.MiniApp = m
		}
		return nil
	})
	return c, err
}

type imMessage struct {
	GUID                 string
	Content              imContent
	DateCreated          time.Time
	Sender               *imAddress
	IsFromMe             bool
	IsAudioMessage       bool
	ThreadOriginatorGUID string
	ReplyTargetGUID      string
	AppliedReactions     []imAppliedReaction
	ChatGUIDs            []string
}

func decodeMessage(b []byte) (*imMessage, error) {
	m := &imMessage{}
	err := pbFields(b, func(f pbField) error {
		var err error
		switch f.num {
		case 1:
			m.GUID = f.str()
		case 2:
			m.Content, err = decodeContent(f.buf)
		case 10:
			m.DateCreated, err = decodeTimestamp(f.buf)
		case 20:
			m.Sender, err = decodeAddress(f.buf)
		case 21:
			m.IsFromMe = f.boolean()
		case 40:
			m.IsAudioMessage = f.boolean()
		case 71:
			m.ThreadOriginatorGUID = f.str()
		case 74:
			m.ReplyTargetGUID = f.str()
		case 90:
			var r imAppliedReaction
			if r, err = decodeAppliedReaction(f.buf); err == nil {
				m.AppliedReactions = append(m.AppliedReactions, r)
			}
		case 92:
			m.ChatGUIDs = append(m.ChatGUIDs, f.str())
		}
		return err
	})
	return m, err
}

// Message field of a MessageResponse or GetMessageResponse, an error when the server sent none
func decodeMessageEnvelope(b []byte) (*imMessage, error) {
	var msg *imMessage
	err := pbFields(b, func(f pbField) error {
		if f.num == 1 && f.isBytes() {
			m, err := decodeMessage(f.buf)
			msg = m
			return err
		}
		return nil
	})
	if err == nil && msg == nil {
		return nil, errMissingField("message")
	}
	return msg, err
}

type imChat struct {
	GUID         string
	DisplayName  string
	IsGroup      bool
	Participants []imAddress
}

func decodeChat(b []byte) (*imChat, error) {
	c := &imChat{}
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			c.GUID = f.str()
		case 4:
			c.DisplayName = f.str()
		case 5:
			c.IsGroup = f.boolean()
		case 10:
			a, err := decodeAddress(f.buf)
			if err != nil {
				return err
			}
			c.Participants = append(c.Participants, *a)
		}
		return nil
	})
	return c, err
}

// Chat field of a CreateChatResponse or GetChatResponse
func decodeChatEnvelope(b []byte) (*imChat, error) {
	var chat *imChat
	err := pbFields(b, func(f pbField) error {
		if f.num == 1 && f.isBytes() {
			c, err := decodeChat(f.buf)
			chat = c
			return err
		}
		return nil
	})
	if err == nil && chat == nil {
		return nil, errMissingField("chat")
	}
	return chat, err
}

type imPollOption struct {
	Text       string
	Identifier string
}

func decodePollOption(b []byte) (imPollOption, error) {
	var o imPollOption
	err := pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			o.Text = f.str()
		case 2:
			o.Identifier = f.str()
		}
		return nil
	})
	return o, err
}

type imPoll struct {
	MessageGUID string
	ChatGUID    string
	Title       string
	Options     []imPollOption
}

// Poll field of a PollResponse
func decodePollEnvelope(b []byte) (*imPoll, error) {
	var poll *imPoll
	err := pbFields(b, func(f pbField) error {
		if f.num != 1 || !f.isBytes() {
			return nil
		}
		p := &imPoll{}
		poll = p
		return pbFields(f.buf, func(pf pbField) error {
			switch pf.num {
			case 1:
				p.MessageGUID = pf.str()
			case 2:
				p.ChatGUID = pf.str()
			case 3:
				p.Title = pf.str()
			case 4:
				o, err := decodePollOption(pf.buf)
				if err != nil {
					return err
				}
				p.Options = append(p.Options, o)
			}
			return nil
		})
	})
	if err == nil && poll == nil {
		return nil, errMissingField("poll")
	}
	return poll, err
}

// Attachment field of an UploadAttachmentResponse
func decodeUploadResponse(b []byte) (imAttachment, error) {
	var att imAttachment
	found := false
	err := pbFields(b, func(f pbField) error {
		if f.num == 1 && f.isBytes() {
			found = true
			a, err := decodeAttachment(f.buf)
			att = a
			return err
		}
		return nil
	})
	if err == nil && !found {
		return att, errMissingField("attachment")
	}
	return att, err
}

// Context every durable change event shares
type eventContext struct {
	ChatGUID   string
	OccurredAt time.Time
	Actor      *imAddress
	IsFromMe   bool
}

func (c *eventContext) decode(f pbField) (bool, error) {
	var err error
	switch f.num {
	case 1:
		c.ChatGUID = f.str()
	case 2:
		c.OccurredAt, err = decodeTimestamp(f.buf)
	case 3:
		c.Actor, err = decodeAddress(f.buf)
	case 4:
		c.IsFromMe = f.boolean()
	default:
		return false, nil
	}
	return true, err
}

// One MessageChangeEvent, Kind naming the change the SDK maps
type messageEvent struct {
	eventContext
	Kind        string
	Message     *imMessage
	MessageGUID string
	PartIndex   *int32
	Reaction    imReaction
}

func decodeMessageEvent(b []byte) (*messageEvent, error) {
	e := &messageEvent{}
	err := pbFields(b, func(f pbField) error {
		if done, err := e.decode(f); done || err != nil {
			return err
		}
		switch f.num {
		case 10:
			e.Kind = "message.received"
			return pbFields(f.buf, func(sf pbField) error {
				if sf.num == 1 && sf.isBytes() {
					m, err := decodeMessage(sf.buf)
					e.Message = m
					return err
				}
				return nil
			})
		case 11:
			e.Kind = "message.edited"
		case 12:
			e.Kind = "message.read"
			return pbFields(f.buf, func(sf pbField) error {
				if sf.num == 1 {
					e.MessageGUID = sf.str()
				}
				return nil
			})
		case 13:
			e.Kind = "message.unsent"
		case 14, 15:
			e.Kind = map[protowire.Number]string{14: "message.reactionAdded", 15: "message.reactionRemoved"}[f.num]
			return pbFields(f.buf, func(sf pbField) error {
				switch sf.num {
				case 1:
					e.MessageGUID = sf.str()
				case 2:
					v := sf.int32()
					e.PartIndex = &v
				case 3:
					r, err := decodeReaction(sf.buf)
					e.Reaction = r
					return err
				}
				return nil
			})
		case 16:
			e.Kind = "message.stickerPlaced"
		}
		return nil
	})
	return e, err
}

// One PollChangeEvent with the delta it carries
type pollEvent struct {
	eventContext
	PollGUID string
	Kind     string
	Title    string
	Options  []imPollOption
	OptionID string
}

func decodePollEvent(b []byte) (*pollEvent, error) {
	e := &pollEvent{}
	err := pbFields(b, func(f pbField) error {
		var err error
		switch f.num {
		case 1:
			e.ChatGUID = f.str()
		case 2:
			e.PollGUID = f.str()
		case 3:
			e.OccurredAt, err = decodeTimestamp(f.buf)
		case 4:
			e.Actor, err = decodeAddress(f.buf)
		case 5:
			e.IsFromMe = f.boolean()
		case 10, 11:
			e.Kind = map[protowire.Number]string{10: "created", 11: "optionAdded"}[f.num]
			err = pbFields(f.buf, func(sf pbField) error {
				switch sf.num {
				case 1:
					e.Title = sf.str()
				case 2:
					o, err := decodePollOption(sf.buf)
					if err != nil {
						return err
					}
					e.Options = append(e.Options, o)
				}
				return nil
			})
		case 12, 13:
			e.Kind = map[protowire.Number]string{12: "voted", 13: "unvoted"}[f.num]
			err = pbFields(f.buf, func(sf pbField) error {
				if sf.num == 1 {
					e.OptionID = sf.str()
				}
				return nil
			})
		}
		return err
	})
	return e, err
}

// One GroupChangeEvent with the change it carries
type groupEvent struct {
	eventContext
	Kind        string
	DisplayName string
	Participant *imAddress
}

func decodeGroupEvent(b []byte) (*groupEvent, error) {
	e := &groupEvent{}
	err := pbFields(b, func(f pbField) error {
		if done, err := e.decode(f); done || err != nil {
			return err
		}
		switch f.num {
		case 10:
			e.Kind = "displayNameChanged"
			return pbFields(f.buf, func(sf pbField) error {
				if sf.num == 1 {
					e.DisplayName = sf.str()
				}
				return nil
			})
		case 11, 12, 13:
			e.Kind = map[protowire.Number]string{11: "participantAdded", 12: "participantRemoved", 13: "participantLeft"}[f.num]
			return pbFields(f.buf, func(sf pbField) error {
				if sf.num == 1 && sf.isBytes() {
					a, err := decodeAddress(sf.buf)
					e.Participant = a
					return err
				}
				return nil
			})
		case 14:
			e.Kind = "iconChanged"
		case 15:
			e.Kind = "iconRemoved"
		}
		return nil
	})
	return e, err
}

// One frame of a Subscribe or CatchUp stream
type streamFrame struct {
	Seq       uint64
	HasSeq    bool
	Heartbeat bool
	Message   *messageEvent
	Poll      *pollEvent
	Group     *groupEvent
	Complete  bool
	Head      uint64
}

// Decodes a Subscribe or CatchUp response frame, the payload field numbers shared across them
func decodeFrame(b []byte) (*streamFrame, error) {
	fr := &streamFrame{}
	err := pbFields(b, func(f pbField) error {
		var err error
		switch f.num {
		case 1:
			fr.Seq, fr.HasSeq = f.val, true
		case 10:
			fr.Message, err = decodeMessageEvent(f.buf)
		case 11:
			fr.Group, err = decodeGroupEvent(f.buf)
		case 12:
			fr.Poll, err = decodePollEvent(f.buf)
		case 20:
			fr.Complete = true
			err = pbFields(f.buf, func(sf pbField) error {
				if sf.num == 1 {
					fr.Head = sf.val
				}
				return nil
			})
		case 99:
			fr.Heartbeat = true
		}
		return err
	})
	return fr, err
}

// Poll and group payloads of a Subscribe frame use field 10 like the message stream
func decodeSubscribeFrame(kind string, b []byte) (*streamFrame, error) {
	fr := &streamFrame{}
	err := pbFields(b, func(f pbField) error {
		var err error
		switch f.num {
		case 1:
			fr.Seq, fr.HasSeq = f.val, true
		case 10:
			switch kind {
			case streamMessages:
				fr.Message, err = decodeMessageEvent(f.buf)
			case streamPolls:
				fr.Poll, err = decodePollEvent(f.buf)
			case streamGroups:
				fr.Group, err = decodeGroupEvent(f.buf)
			}
		case 99:
			fr.Heartbeat = true
		}
		return err
	})
	return fr, err
}

// One DownloadAttachmentResponse frame, primary bytes only
func decodeDownloadFrame(b []byte) (primary []byte, err error) {
	err = pbFields(b, func(f pbField) error {
		if f.num == 2 && f.isBytes() {
			primary = f.buf
		}
		return nil
	})
	return primary, err
}

// Icon bytes and mime of a GetIconResponse
func decodeIcon(b []byte) (data []byte, mime string, err error) {
	err = pbFields(b, func(f pbField) error {
		switch f.num {
		case 1:
			data = f.buf
		case 2:
			mime = f.str()
		}
		return nil
	})
	return data, mime, err
}
