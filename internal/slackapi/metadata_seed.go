package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/store"
	"github.com/slack-go/slack"
)

// ApprovedChannel holds the channel facts in a previously authorized inventory.
type ApprovedChannel struct {
	ID, Name, Kind                                string
	Member, Archived, Shared, External, OrgShared bool
}

// An omitted visibility flag cannot qualify a channel as explicitly unshared.
func (ch *ApprovedChannel) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{"member", "archived", "shared", "external", "orgShared"} {
		raw := string(fields[key])
		if raw != "true" && raw != "false" {
			return errors.New("approved channel inventory requires explicit membership and sharing flags")
		}
	}
	type definition ApprovedChannel
	return json.Unmarshal(data, (*definition)(ch))
}

type MetadataSeedChannel struct {
	Channel   store.Channel
	Admission string
}

// MetadataSeed reads only the authenticated reader identity and the public/private
// channel directory. Unexpected channels never become archive rows.
func (c *Client) MetadataSeed(ctx context.Context, workspace, app, user string, expected []ApprovedChannel) (store.Workspace, []MetadataSeedChannel, error) {
	if workspace == "" || app == "" || user == "" || len(expected) == 0 {
		return store.Workspace{}, nil, errors.New("metadata seed requires an admitted reader and channels")
	}
	admitted := make(map[string]ApprovedChannel, len(expected))
	for _, ch := range expected {
		_, dup := admitted[ch.ID]
		if dup || len(ch.ID) < 2 || (ch.ID[0] != 'C' && ch.ID[0] != 'G') || ch.Name == "" || (ch.Kind != "public_channel" && ch.Kind != "private_channel") || !ch.Member || ch.Archived {
			return store.Workspace{}, nil, errors.New("invalid metadata channel admission")
		}
		admitted[ch.ID] = ch
	}
	token, err := c.acquisitionToken(SourceBot)
	if err != nil {
		return store.Workspace{}, nil, err
	}
	auth, err := c.seedReader(ctx, token, workspace, app, user)
	if err != nil {
		return store.Workspace{}, nil, err
	}
	now := c.now()
	team := store.Workspace{ID: workspace, Name: auth.Team, EnterpriseID: auth.EnterpriseID, RawJSON: store.MarshalRaw(map[string]string{"id": workspace, "name": auth.Team}), UpdatedAt: now}
	var out []MetadataSeedChannel
	seen := map[string]bool{}
	err = visitWorkspaceChannels(ctx, c, token, workspace, func(page []json.RawMessage) error {
		for _, raw := range page {
			var ref struct{ ID string }
			if err := json.Unmarshal(raw, &ref); err != nil {
				return err
			}
			want, ok := admitted[ref.ID]
			if !ok {
				continue
			}
			if seen[ref.ID] {
				return errors.New("duplicate admitted channel in provider directory")
			}
			seen[ref.ID] = true
			item, err := projectMetadataChannel(raw, want, workspace, now)
			if err != nil {
				return err
			}
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		return store.Workspace{}, nil, err
	}
	if len(out) != len(expected) {
		return store.Workspace{}, nil, errors.New("admitted channel missing from provider directory")
	}
	return team, out, nil
}

func (c *Client) seedReader(ctx context.Context, token, workspace, app, user string) (*authIdentity, error) {
	auth, err := c.authTestIdentity(ctx, token)
	if err != nil {
		return nil, err
	}
	if auth.TeamID != workspace || auth.UserID != user || auth.BotID == "" {
		return nil, errors.New("metadata reader identity mismatch")
	}
	appID := auth.AppID
	if appID == "" {
		bot, err := retry(ctx, c.sleep, 3, func() (slack.Bot, error) {
			var resp struct {
				slack.SlackResponse
				Bot slack.Bot `json:"bot"`
			}
			if _, err := c.postSlackFormLimit(ctx, token, "bots.info", url.Values{"bot": {auth.BotID}}, &resp, 1<<20); err != nil {
				return slack.Bot{}, err
			}
			return resp.Bot, nativeResponseSuccess("bots.info", resp.SlackResponse)
		})
		if err != nil {
			return nil, err
		}
		if bot.ID != auth.BotID || bot.UserID != user || bot.Deleted {
			return nil, errors.New("metadata reader bot mismatch")
		}
		appID = bot.AppID
	}
	if appID != app {
		return nil, errors.New("metadata reader app mismatch")
	}
	return auth, nil
}

func projectMetadataChannel(raw json.RawMessage, want ApprovedChannel, workspace string, now time.Time) (MetadataSeedChannel, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return MetadataSeedChannel{}, err
	}
	// Omit text, member lists, the optional latest message, and all
	// unknown fields. Retain the provider's original identity facts.
	metadata := map[string]json.RawMessage{}
	for _, key := range strings.Fields("id channel_id team_id workspace_id context_team_id name created is_channel is_group is_im is_mpim is_private is_shared is_ext_shared is_org_shared is_member is_archived is_general") {
		if value, ok := fields[key]; ok {
			metadata[key] = value
		}
	}
	projected, err := json.Marshal(metadata)
	if err != nil {
		return MetadataSeedChannel{}, err
	}
	var native slack.Channel
	if err := json.Unmarshal(projected, &native); err != nil {
		return MetadataSeedChannel{}, errors.New("unexpected channel metadata types")
	}
	ch := toStoreChannel(workspace, native, now)
	ch.RawJSON = string(projected)
	parent := string(fields["parent_conversation"])
	if (parent != "" && parent != "null" && parent != `""`) || ch.Name != want.Name || ch.Kind != want.Kind || ch.IsShared != want.Shared {
		return MetadataSeedChannel{}, errors.New("admitted channel metadata changed")
	}
	for key, expected := range map[string]bool{"is_member": want.Member, "is_archived": want.Archived, "is_ext_shared": want.External, "is_org_shared": want.OrgShared} {
		value := string(fields[key])
		if value != strconv.FormatBool(expected) {
			return MetadataSeedChannel{}, errors.New("admitted channel membership or sharing changed")
		}
	}
	facts := store.NativeChannelFacts{ChannelID: ch.ID, WorkspaceID: workspace, Kind: ch.Kind, RawJSON: string(raw)}
	if ch.IsPrivate {
		facts.IsPrivate = 1
	}
	if ch.IsShared {
		facts.IsShared = 1
	}
	// Validate the original object, before projecting away unknown and
	// duplicate keys. Every identity/flag in the digest is retained.
	digest, err := store.NativeChannelAdmissionSHA256(facts)
	if err != nil {
		return MetadataSeedChannel{}, err
	}
	return MetadataSeedChannel{ch, digest}, nil
}
