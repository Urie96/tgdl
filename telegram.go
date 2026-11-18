package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

// ErrNotAuthorized is returned when the session is missing and the process is
// not allowed to log in interactively (i.e. running as a service).
var ErrNotAuthorized = errors.New(
	"session is not authorized: run `tgdl login` once to create " +
		"the session file, then start the service")

// newClient builds the gotd client.
//
// The device and transport configuration emulates Telegram Desktop, which is
// the least suspicious fingerprint for a long-running downloader.
func newClient(cfg *Config) *telegram.Client {
	return telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionFile},
		Device:         telegram.DeviceTDesktopWindows(),
		Resolver:       telegram.TDesktopResolver(),
		DCList:         dcs.Prod(),
	})
}

// login runs an interactive authorization flow and stores the session.
func login(ctx context.Context, client *telegram.Client, cfg *Config) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("auth status: %w", err)
	}
	if status.Authorized {
		user, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("get self: %w", err)
		}
		fmt.Printf("already authorized as %s (@%s)\n", displayName(user), user.Username)
		return nil
	}

	p := &prompter{
		cfg:         cfg,
		in:          bufio.NewReader(os.Stdin),
		interactive: true,
	}
	if err := client.Auth().IfNecessary(ctx, auth.NewFlow(p, auth.SendCodeOptions{})); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	user, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("get self: %w", err)
	}
	fmt.Printf("logged in as %s (@%s), session saved to %s\n",
		displayName(user), user.Username, cfg.SessionFile)
	return nil
}

// ensureAuthorized fails fast when the session is missing.
func ensureAuthorized(ctx context.Context, client *telegram.Client) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("auth status: %w", err)
	}
	if !status.Authorized {
		return ErrNotAuthorized
	}
	return nil
}

func displayName(u *tg.User) string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if n == "" {
		return fmt.Sprintf("user %d", u.ID)
	}
	return n
}

// prompter implements auth.UserAuthenticator on the terminal.
type prompter struct {
	cfg         *Config
	in          *bufio.Reader
	interactive bool
}

func (p *prompter) Phone(context.Context) (string, error) {
	if p.cfg.Phone != "" {
		return p.cfg.Phone, nil
	}
	if !p.interactive {
		return "", ErrNotAuthorized
	}
	fmt.Print("phone (international, e.g. +8613800000000): ")
	s, err := p.readLine()
	if err != nil {
		return "", err
	}
	p.cfg.Phone = strings.TrimSpace(s)
	return p.cfg.Phone, nil
}

func (p *prompter) Password(context.Context) (string, error) {
	if p.cfg.Password != "" {
		return p.cfg.Password, nil
	}
	if !p.interactive {
		return "", auth.ErrPasswordNotProvided
	}
	fmt.Print("2FA password: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		p.cfg.Password = strings.TrimSpace(string(b))
		return p.cfg.Password, nil
	}
	s, err := p.readLine()
	if err != nil {
		return "", err
	}
	p.cfg.Password = strings.TrimSpace(s)
	return p.cfg.Password, nil
}

func (p *prompter) Code(_ context.Context, sent *tg.AuthSentCode) (string, error) {
	if !p.interactive {
		return "", ErrNotAuthorized
	}
	kind := "Telegram"
	if sent != nil {
		switch sent.Type.(type) {
		case *tg.AuthSentCodeTypeSMS:
			kind = "SMS"
		case *tg.AuthSentCodeTypeCall:
			kind = "phone call"
		case *tg.AuthSentCodeTypeApp:
			kind = "Telegram app"
		case *tg.AuthSentCodeTypeEmailCode:
			kind = "email"
		case *tg.AuthSentCodeTypeFlashCall:
			kind = "flash call"
		}
	}
	fmt.Printf("login code (sent via %s): ", kind)
	s, err := p.readLine()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func (p *prompter) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	return &auth.SignUpRequired{}
}

func (p *prompter) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("sign up is not supported, use an existing account")
}

func (p *prompter) readLine() (string, error) {
	s, err := p.in.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// peerKind distinguishes the two MTProto peer types a download source can be.
//
// Telegram merges broadcast channels and supergroups into one TL type
// (tg.Channel); basic groups are a separate one (tg.Chat) with a different
// input peer and no access hash.
type peerKind string

const (
	kindChannel peerKind = "channel"
	kindChat    peerKind = "chat"
)

// channelRef is a resolved source peer.
type channelRef struct {
	ID         int64
	AccessHash int64
	Kind       peerKind
	Username   string
	Title      string
	Folder     string
	// Spec is the config entry this channel was resolved from.
	Spec Channel
}

// InputPeer returns the peer input object for this source.
//
// Basic groups need InputPeerChat and carry no access hash; channels and
// supergroups need InputPeerChannel with one.
func (c channelRef) InputPeer() tg.InputPeerClass {
	if c.Kind == kindChat {
		return &tg.InputPeerChat{ChatID: c.ID}
	}
	return &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: c.AccessHash}
}

func (c channelRef) String() string {
	name := c.Username
	if name != "" {
		name = "@" + name
	} else if c.Title != "" {
		name = c.Title
	}
	if name == "" {
		name = strconv.FormatInt(c.ID, 10)
	}
	if c.Kind == kindChat {
		return fmt.Sprintf("%s (group %d)", name, c.ID)
	}
	return fmt.Sprintf("%s (%d)", name, c.ID)
}

// resolveChannel turns a config entry into a channelRef, using the local cache
// first and Telegram only when needed.
func resolveChannel(ctx context.Context, api *tg.Client, st *Store, spec Channel) (channelRef, error) {
	username, id, kind, err := parseChannelSpec(spec.Channel)
	if err != nil {
		return channelRef{}, err
	}

	if username != "" {
		if cached, err := st.ChannelByUsername(username); err == nil && usableCache(cached, kindChannel) {
			return refFromState(cached, spec), nil
		}
		return resolveUsername(ctx, api, st, username, spec)
	}

	if cached, err := st.Channel(id); err == nil && usableCache(cached, kind) {
		return refFromState(cached, spec), nil
	}

	if kind == kindChat {
		return resolveChat(ctx, api, st, id, spec)
	}

	// We only have an id: ask Telegram directly. This works for channels the
	// account is a member of in most cases; if it fails the user must supply
	// the @username once.
	ch, err := fetchChannel(ctx, api, &tg.InputChannel{ChannelID: id})
	if err != nil {
		return channelRef{}, fmt.Errorf(
			"cannot resolve channel id %d without a username: %w\n"+
				"hint: use \"channel: @username\" the first time, or ensure the account is a member",
			id, err)
	}
	return persistChannel(st, ch, spec)
}

// usableCache reports whether a stored row can be used without asking Telegram.
func usableCache(st *ChannelState, kind peerKind) bool {
	if st == nil {
		return false
	}
	if peerKind(st.Kind) != kind {
		return false
	}
	// Basic groups have no access hash, so only channels need one.
	return kind == kindChat || st.AccessHash != 0
}

// resolveChat resolves a basic group (tg.Chat).
func resolveChat(ctx context.Context, api *tg.Client, st *Store, id int64, spec Channel) (channelRef, error) {
	chat, err := fetchChat(ctx, api, id)
	if err != nil {
		return channelRef{}, fmt.Errorf("resolve group %d: %w", id, err)
	}

	// A basic group upgraded to a supergroup hands over the channel to follow.
	// The old id stops working, so follow the migration transparently.
	if migrated, ok := chat.MigratedTo.(*tg.InputChannel); ok && migrated != nil {
		ch, err := fetchChannel(ctx, api, migrated)
		if err != nil {
			return channelRef{}, fmt.Errorf(
				"group %d was migrated to supergroup %d: %w", id, migrated.ChannelID, err)
		}
		slog.Default().Info("group migrated to supergroup",
			"group", id, "channel", ch.ID)
		return persistChannel(st, ch, spec)
	}
	return persistChat(st, chat, spec)
}

func resolveUsername(ctx context.Context, api *tg.Client, st *Store, username string, spec Channel) (channelRef, error) {
	res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return channelRef{}, fmt.Errorf("resolve @%s: %w", username, err)
	}
	for _, c := range res.Chats {
		if ch, ok := c.(*tg.Channel); ok {
			return persistChannel(st, ch, spec)
		}
	}
	return channelRef{}, fmt.Errorf("resolve @%s: username is not a channel", username)
}

func fetchChannel(ctx context.Context, api *tg.Client, in tg.InputChannelClass) (*tg.Channel, error) {
	res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{in})
	if err != nil {
		return nil, err
	}
	chats, ok := res.(*tg.MessagesChats)
	if !ok {
		return nil, fmt.Errorf("unexpected response %T", res)
	}
	for _, c := range chats.Chats {
		if ch, ok := c.(*tg.Channel); ok {
			return ch, nil
		}
	}
	return nil, errors.New("channel not found")
}

// fetchChat resolves a basic group by id.
func fetchChat(ctx context.Context, api *tg.Client, id int64) (*tg.Chat, error) {
	res, err := api.MessagesGetChats(ctx, []int64{id})
	if err != nil {
		return nil, err
	}
	chats, ok := res.(*tg.MessagesChats)
	if !ok {
		return nil, fmt.Errorf("unexpected response %T", res)
	}
	for _, c := range chats.Chats {
		if chat, ok := c.(*tg.Chat); ok {
			return chat, nil
		}
	}
	return nil, errors.New("group not found (is the account a member?)")
}

// persistChannel stores a resolved channel (or supergroup).
func persistChannel(st *Store, ch *tg.Channel, spec Channel) (channelRef, error) {
	return persistPeer(st, channelRef{
		ID:         ch.ID,
		Kind:       kindChannel,
		AccessHash: ch.AccessHash,
		Username:   ch.Username,
		Title:      ch.Title,
		Spec:       spec,
	})
}

// persistChat stores a resolved basic group.
func persistChat(st *Store, chat *tg.Chat, spec Channel) (channelRef, error) {
	return persistPeer(st, channelRef{
		ID:    chat.ID,
		Kind:  kindChat,
		Title: chat.Title,
		Spec:  spec,
	})
}

// persistPeer derives the folder name and writes the channel row, preserving
// any progress cursors from a previous run.
func persistPeer(st *Store, ref channelRef) (channelRef, error) {
	folder := ref.Spec.Dir
	if folder == "" {
		switch {
		case ref.Username != "":
			folder = sanitize(ref.Username)
		case ref.Title != "":
			folder = sanitize(ref.Title)
		default:
			folder = fmt.Sprintf("channel_%d", ref.ID)
		}
	}
	ref.Folder = folder

	var prev ChannelState
	if p, err := st.Channel(ref.ID); err == nil && p != nil && peerKind(p.Kind) == ref.Kind {
		prev = *p
	}
	if err := st.PutChannel(&ChannelState{
		ChannelID:      ref.ID,
		Kind:           string(ref.Kind),
		AccessHash:     ref.AccessHash,
		Username:       ref.Username,
		Title:          ref.Title,
		Folder:         ref.Folder,
		Head:           prev.Head,
		BackfillOffset: prev.BackfillOffset,
		BackfillDone:   prev.BackfillDone,
	}); err != nil {
		return ref, err
	}
	return ref, nil
}

func refFromState(s *ChannelState, spec Channel) channelRef {
	folder := spec.Dir
	if folder == "" {
		folder = s.Folder
	}
	if folder == "" {
		switch {
		case s.Username != "":
			folder = sanitize(s.Username)
		case s.Title != "":
			folder = sanitize(s.Title)
		default:
			folder = fmt.Sprintf("channel_%d", s.ChannelID)
		}
	}
	kind := peerKind(s.Kind)
	if kind == "" {
		kind = kindChannel
	}
	return channelRef{
		ID:         s.ChannelID,
		Kind:       kind,
		AccessHash: s.AccessHash,
		Username:   s.Username,
		Title:      s.Title,
		Folder:     folder,
		Spec:       spec,
	}
}

// parseChannelSpec accepts "@name", "t.me/name", "-1001234567890" (channel or
// supergroup) and "-1234567890" (basic group).
//
// Basic groups are returned with kind == kindChat. Usernames only exist on
// channels and supergroups, because MTProto's Chat has no username field.
func parseChannelSpec(raw string) (username string, id int64, kind peerKind, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", 0, "", errors.New("empty channel")
	}

	for _, prefix := range []string{"https://t.me/", "http://t.me/", "t.me/", "telegram.me/"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimPrefix(s, "@")

	if s == "" {
		return "", 0, "", fmt.Errorf("invalid channel %q", raw)
	}
	if !isNumeric(s) {
		return strings.ToLower(s), 0, kindChannel, nil
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid channel id %q: %w", raw, err)
	}
	if n >= 0 {
		return "", n, kindChannel, nil
	}

	// Same convention TDLib uses: -100<id> is a channel/supergroup, every
	// other negative id is a basic group.
	m := -n
	const channelPrefix int64 = 1_000_000_000_000
	if m > channelPrefix {
		return "", m - channelPrefix, kindChannel, nil
	}
	return "", m, kindChat, nil
}
