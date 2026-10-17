// Package slackexport reads a standard Slack workspace export directory.
package slackexport

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TS is a Slack message timestamp such as "1492622693.330270". It is unique
// within a channel and doubles as the message ID.
type TS string

// UnmarshalJSON accepts both the usual string form and the bare numbers that
// appear in some older attachment objects.
func (t *TS) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*t = TS(s)
		return nil
	}
	if string(data) == "null" {
		*t = ""
		return nil
	}
	*t = TS(data)
	return nil
}

// Parse splits the timestamp into seconds and microseconds.
func (t TS) Parse() (sec, usec int64, err error) {
	s := string(t)
	whole, frac, _ := strings.Cut(s, ".")
	sec, err = strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid ts %q: %w", s, err)
	}
	if frac != "" {
		frac = (frac + "000000")[:6]
		usec, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid ts %q: %w", s, err)
		}
	}
	return sec, usec, nil
}

// Micros returns the timestamp in microseconds, or 0 if it is malformed.
func (t TS) Micros() int64 {
	sec, usec, err := t.Parse()
	if err != nil {
		return 0
	}
	return sec*1_000_000 + usec
}

// Millis returns the timestamp in milliseconds, as used by Matrix origin_server_ts.
func (t TS) Millis() int64 {
	return t.Micros() / 1000
}

// Time converts the timestamp to a time.Time.
func (t TS) Time() time.Time {
	return time.UnixMicro(t.Micros())
}

// Less orders timestamps numerically.
func (t TS) Less(o TS) bool {
	return t.Micros() < o.Micros()
}

type User struct {
	ID        string      `json:"id"`
	TeamID    string      `json:"team_id"`
	Name      string      `json:"name"`
	Deleted   bool        `json:"deleted"`
	RealName  string      `json:"real_name"`
	IsBot     bool        `json:"is_bot"`
	IsAppUser bool        `json:"is_app_user"`
	Profile   UserProfile `json:"profile"`
}

// DisplayName picks the best human-readable name for the user.
func (u *User) DisplayName() string {
	for _, n := range []string{u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name} {
		if n != "" {
			return n
		}
	}
	return u.ID
}

type UserProfile struct {
	RealName      string `json:"real_name"`
	DisplayName   string `json:"display_name"`
	FirstName     string `json:"first_name"`
	ImageOriginal string `json:"image_original"`
	Image512      string `json:"image_512"`
	Image192      string `json:"image_192"`
	Image72       string `json:"image_72"`
	BotID         string `json:"bot_id"`
	AvatarHash    string `json:"avatar_hash"`
}

// BestImage returns the largest available avatar URL.
func (p *UserProfile) BestImage() string {
	for _, u := range []string{p.ImageOriginal, p.Image512, p.Image192, p.Image72} {
		if u != "" {
			return u
		}
	}
	return ""
}

type Channel struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Created    int64    `json:"created"`
	Creator    string   `json:"creator"`
	IsArchived bool     `json:"is_archived"`
	IsGeneral  bool     `json:"is_general"`
	Members    []string `json:"members"`
	Topic      TextMeta `json:"topic"`
	Purpose    TextMeta `json:"purpose"`
}

type TextMeta struct {
	Value   string `json:"value"`
	Creator string `json:"creator"`
	LastSet int64  `json:"last_set"`
}

type Message struct {
	Type         string       `json:"type"`
	Subtype      string       `json:"subtype"`
	TS           TS           `json:"ts"`
	User         string       `json:"user"`
	BotID        string       `json:"bot_id"`
	Username     string       `json:"username"`
	Text         string       `json:"text"`
	Blocks       []Block      `json:"blocks"`
	Attachments  []Attachment `json:"attachments"`
	Files        []File       `json:"files"`
	Upload       bool         `json:"upload"`
	ThreadTS     TS           `json:"thread_ts"`
	ParentUserID string       `json:"parent_user_id"`
	ReplyCount   int          `json:"reply_count"`
	Reactions    []Reaction   `json:"reactions"`
	Edited       *Edited      `json:"edited"`
	Hidden       bool         `json:"hidden"`
	Icons        *Icons       `json:"icons"`
	BotProfile   *BotProfile  `json:"bot_profile"`
	UserProfile  *MsgProfile  `json:"user_profile"`
	Inviter      string       `json:"inviter"`
	ItemType     string       `json:"item_type"`
	Name         string       `json:"name"`
	OldName      string       `json:"old_name"`
	Topic        string       `json:"topic"`
	Purpose      string       `json:"purpose"`

	// Legacy file_comment fields.
	Comment *FileComment `json:"comment"`
	File    *File        `json:"file"`
}

// IsThreadReply reports whether the message lives inside a thread but is not its root.
func (m *Message) IsThreadReply() bool {
	return m.ThreadTS != "" && m.ThreadTS != m.TS
}

// SenderID returns the Slack user or bot ID that sent the message.
func (m *Message) SenderID() string {
	if m.User != "" {
		return m.User
	}
	if m.BotID != "" {
		return m.BotID
	}
	return ""
}

type Edited struct {
	User string `json:"user"`
	TS   TS     `json:"ts"`
}

type Reaction struct {
	Name  string   `json:"name"`
	Users []string `json:"users"`
	Count int      `json:"count"`
}

type Icons struct {
	Emoji   string `json:"emoji"`
	Image36 string `json:"image_36"`
	Image48 string `json:"image_48"`
	Image64 string `json:"image_64"`
	Image72 string `json:"image_72"`
}

type BotProfile struct {
	ID      string `json:"id"`
	AppID   string `json:"app_id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	Icons   struct {
		Image36 string `json:"image_36"`
		Image48 string `json:"image_48"`
		Image72 string `json:"image_72"`
	} `json:"icons"`
}

type MsgProfile struct {
	RealName    string `json:"real_name"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	Image72     string `json:"image_72"`
}

type FileComment struct {
	ID        string `json:"id"`
	Created   int64  `json:"created"`
	Timestamp int64  `json:"timestamp"`
	User      string `json:"user"`
	Comment   string `json:"comment"`
}

type File struct {
	ID                 string `json:"id"`
	Created            int64  `json:"created"`
	Timestamp          int64  `json:"timestamp"`
	Name               string `json:"name"`
	Title              string `json:"title"`
	Mimetype           string `json:"mimetype"`
	Filetype           string `json:"filetype"`
	PrettyType         string `json:"pretty_type"`
	User               string `json:"user"`
	Mode               string `json:"mode"`
	Size               int64  `json:"size"`
	IsExternal         bool   `json:"is_external"`
	ExternalType       string `json:"external_type"`
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
	Permalink          string `json:"permalink"`
	OriginalW          int    `json:"original_w"`
	OriginalH          int    `json:"original_h"`
	ImageExifRotation  int    `json:"image_exif_rotation"`
	Preview            string `json:"preview"`
	Thumb360           string `json:"thumb_360"`
	ThumbVideo         string `json:"thumb_video"`
	ThumbPDF           string `json:"thumb_pdf"`
	DurationMS         int64  `json:"duration_ms"`
}

// DownloadURL returns the URL whose path mirrors the local media copy.
func (f *File) DownloadURL() string {
	if f.URLPrivateDownload != "" {
		return f.URLPrivateDownload
	}
	return f.URLPrivate
}

type Attachment struct {
	ID          int               `json:"id"`
	Fallback    string            `json:"fallback"`
	Color       string            `json:"color"`
	Pretext     string            `json:"pretext"`
	AuthorName  string            `json:"author_name"`
	AuthorLink  string            `json:"author_link"`
	AuthorID    string            `json:"author_id"`
	AuthorSub   string            `json:"author_subname"`
	Title       string            `json:"title"`
	TitleLink   string            `json:"title_link"`
	Text        string            `json:"text"`
	Fields      []AttachmentField `json:"fields"`
	ImageURL    string            `json:"image_url"`
	ImageWidth  int               `json:"image_width"`
	ImageHeight int               `json:"image_height"`
	ImageBytes  int64             `json:"image_bytes"`
	ThumbURL    string            `json:"thumb_url"`
	ThumbWidth  int               `json:"thumb_width"`
	ThumbHeight int               `json:"thumb_height"`
	VideoHTML   string            `json:"video_html"`
	Footer      string            `json:"footer"`
	ServiceName string            `json:"service_name"`
	ServiceIcon string            `json:"service_icon"`
	FromURL     string            `json:"from_url"`
	OriginalURL string            `json:"original_url"`
	IsMsgUnfurl bool              `json:"is_msg_unfurl"`
	IsShare     bool              `json:"is_share"`
	ChannelID   string            `json:"channel_id"`
	TS          TS                `json:"ts"`
	MrkdwnIn    []string          `json:"mrkdwn_in"`
	Blocks      []Block           `json:"message_blocks"`
	Files       []File            `json:"files"`
}

// IsUnfurl reports whether Slack generated the attachment as a link preview.
func (a *Attachment) IsUnfurl() bool {
	return a.FromURL != "" || a.OriginalURL != ""
}

type AttachmentField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// Block is a Block Kit layout block. Only the fields used by rich_text, image,
// context and section blocks are modelled.
type Block struct {
	Type      string    `json:"type"`
	BlockID   string    `json:"block_id"`
	Elements  []Element `json:"elements"`
	Text      *TextObj  `json:"text"`
	Fields    []TextObj `json:"fields"`
	ImageURL  string    `json:"image_url"`
	AltText   string    `json:"alt_text"`
	Title     *TextObj  `json:"title"`
	ImageW    int       `json:"image_width"`
	ImageH    int       `json:"image_height"`
	ImageSize int64     `json:"image_bytes"`
}

type TextObj struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Element is a node in a rich_text tree (or a context block element).
type Element struct {
	Type        string    `json:"type"`
	Elements    []Element `json:"elements"`
	Text        string    `json:"text"`
	Style       Style     `json:"style"`
	Name        string    `json:"name"`
	Unicode     string    `json:"unicode"`
	SkinTone    int       `json:"skin_tone"`
	URL         string    `json:"url"`
	UserID      string    `json:"user_id"`
	ChannelID   string    `json:"channel_id"`
	UsergroupID string    `json:"usergroup_id"`
	Range       string    `json:"range"`
	Value       string    `json:"value"`
	Indent      int       `json:"indent"`
	Offset      int       `json:"offset"`
	Border      int       `json:"border"`
	ImageURL    string    `json:"image_url"`
	AltText     string    `json:"alt_text"`
}

// Style holds inline text styling. Slack reuses the "style" key for the list
// style of rich_text_list ("bullet"/"ordered"), which is stored in ListStyle.
type Style struct {
	Bold      bool
	Italic    bool
	Strike    bool
	Code      bool
	ListStyle string
}

func (s *Style) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &s.ListStyle)
	}
	var raw struct {
		Bold   bool `json:"bold"`
		Italic bool `json:"italic"`
		Strike bool `json:"strike"`
		Code   bool `json:"code"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Bold, s.Italic, s.Strike, s.Code = raw.Bold, raw.Italic, raw.Strike, raw.Code
	return nil
}

type Canvas struct {
	ID                 string `json:"id"`
	Created            int64  `json:"created"`
	Name               string `json:"name"`
	Title              string `json:"title"`
	Mimetype           string `json:"mimetype"`
	User               string `json:"user"`
	Size               int64  `json:"size"`
	URLPrivateDownload string `json:"url_private_download"`
	Shares             []struct {
		Team    string `json:"team"`
		Channel string `json:"channel"`
	} `json:"shares"`
}
