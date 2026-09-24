const fs = require("fs");
const v8 = require("v8");

const inputPath = process.argv[1];
if (!inputPath) {
  throw new Error("decoded V8 payload path is required");
}

const buffer = fs.readFileSync(inputPath);
const value = v8.deserialize(buffer);

const rawChannelEntries = Object.entries(value.channels || {});
const channelEntries = rawChannelEntries.filter(
  ([, entry]) => entry && typeof entry === "object" && entry.id
);
const channels = channelEntries.map(([, entry]) => entry);
const channelProvenance = channelEntries.map(([key, entry]) => ({
  key: Array.isArray(value.channels) ? "" : key,
  aliases: channelAliases(entry, true),
}));
// Sparse keyed entries remain exclusion evidence even when legacy selection
// cannot retain a channel payload. Keep only identities and native type flags.
const channelObservations = rawChannelEntries.map(([key, entry]) => {
  const channel = entry && typeof entry === "object" ? entry : {};
  return {
    key: Array.isArray(value.channels) ? "" : key,
    id: typeof channel.id === "string" ? channel.id : "",
    aliases: channelAliases(channel, true),
    context_team_id: typeof channel.context_team_id === "string" ? channel.context_team_id : "",
    is_channel: channel.is_channel === true,
    is_group: channel.is_group === true,
    is_im: channel.is_im === true,
    is_mpim: channel.is_mpim === true,
    is_private: channel.is_private === true,
  };
});
const members = Object.values(value.members || {}).filter(
  (entry) => entry && typeof entry === "object" && entry.id
);
const messages = [];
const seenMessages = new Set();
const messageProvenance = new Map();
const evidenceVisits = new WeakMap();
const workspaceId =
  value.selfTeamIds?.teamId ||
  value.selfTeamIds?.defaultWorkspaceId ||
  value.bootData?.team_id ||
  "";
const userId = value.bootData?.user_id || "";

function looksLikeMessage(entry) {
  if (!entry || typeof entry !== "object") {
    return false;
  }
  if (entry.__proto__ && entry.__proto__ !== Object.prototype && !Array.isArray(entry)) {
    return false;
  }
  const hasTimestamp =
    typeof entry.ts === "string" ||
    typeof entry.ts === "number" ||
    typeof entry.thread_ts === "string" ||
    typeof entry.thread_ts === "number";
  if (!hasTimestamp) {
    return false;
  }
  return (
    entry.type === "message" ||
    typeof entry.text === "string" ||
    typeof entry.subtype === "string" ||
    typeof entry.reply_count === "number" ||
    typeof entry.user === "string" ||
    typeof entry.parent_user_id === "string"
  );
}

function channelAliases(entry, includeID = false) {
  return [...new Set([
    ...(includeID ? [entry.id] : []),
    entry.channel, entry.channel_id, entry.conversation, entry.conversation_id,
  ].filter((id) => typeof id === "string" && id.trim() !== ""))];
}

function pushMessage(entry, fallbackChannel, fallbackTS, context, select) {
  if (!looksLikeMessage(entry)) {
    return;
  }
  const channel =
    entry.channel || entry.channel_id || entry.conversation || entry.conversation_id || fallbackChannel;
  const ts =
    entry.ts !== undefined && entry.ts !== null && entry.ts !== ""
      ? String(entry.ts)
      : fallbackTS !== undefined && fallbackTS !== null && fallbackTS !== ""
        ? String(fallbackTS)
        : "";
  if (!channel || !ts) {
    return;
  }
  const key = `${channel}|${ts}`;
  // Observe every alias/container before duplicate selection discards a payload.
  const aliases = channelAliases(entry);
  const evidence = messageProvenance.get(key) || {
    channel, ts, aliases: [], containers: [], conflict: false, supported: false,
  };
  evidence.aliases = [...new Set([...evidence.aliases, ...aliases])];
  evidence.containers = [...new Set([...evidence.containers, context.channel])];
  evidence.conflict ||= [...aliases, context.channel]
    .some((id) => id && id !== channel);
  messageProvenance.set(key, evidence);
  if (!select || seenMessages.has(key)) {
    return;
  }
  seenMessages.add(key);
  evidence.supported = context.supported;
  messages.push({
    ...entry,
    channel,
    ts,
    thread_ts:
      entry.thread_ts !== undefined && entry.thread_ts !== null && entry.thread_ts !== ""
        ? String(entry.thread_ts)
        : "",
    latest_reply:
      entry.latest_reply !== undefined && entry.latest_reply !== null && entry.latest_reply !== ""
        ? String(entry.latest_reply)
        : "",
  });
}

function walkMessages(node, fallbackChannel, seenNodes, context, ancestors, select = true) {
  if (!node || typeof node !== "object") {
    return;
  }
  if (ancestors.has(node)) {
    return;
  }
  // Keep legacy selection while revisiting shared nodes for identity evidence.
  select = select && !seenNodes.has(node);
  const contextKey = JSON.stringify([context.channel, fallbackChannel, context.supported]);
  const visited = evidenceVisits.get(node) || new Set();
  // Revisit shared nodes for distinct identity evidence, not every DAG path.
  // Parent timestamp-key evidence is already collected before this recursion.
  if (!select && visited.has(contextKey)) return;
  visited.add(contextKey);
  evidenceVisits.set(node, visited);
  if (select) seenNodes.add(node);
  ancestors.add(node);
  if (Array.isArray(node)) {
    for (const entry of node) {
      walkMessages(entry, fallbackChannel, seenNodes, context, ancestors, select);
    }
    ancestors.delete(node);
    return;
  }

  pushMessage(node, fallbackChannel, undefined, context, select);

  for (const [key, child] of Object.entries(node)) {
    if (key === "attachments") {
      continue;
    }
    const nextChannel =
      key.startsWith("C") || key.startsWith("G") || key.startsWith("D") ? key : fallbackChannel;
    const nextFallbackTS =
      child && typeof child === "object" && !Array.isArray(child) && child.ts === undefined
        ? key
        : undefined;
    const childContext = {
      channel: context.channel,
      // Supported cache containers never regain trust below an arbitrary field.
      supported: context.supported &&
        (/^\d+(?:\.\d+)?$/.test(key) || key === "replies" || key === "messages"),
    };
    if (looksLikeMessage(child)) {
      pushMessage(child, nextChannel, nextFallbackTS, childContext, select);
    }
    walkMessages(child, nextChannel, seenNodes, childContext, ancestors, select);
  }
  ancestors.delete(node);
}

for (const [channelID, byTS] of Object.entries(value.messages || {})) {
  walkMessages(byTS, channelID, new WeakSet(), { channel: channelID, supported: /^[CDG]/.test(channelID) }, new WeakSet());
}

for (const [channelID, threadState] of Object.entries(value.threads || {})) {
  walkMessages(threadState, channelID, new WeakSet(), { channel: channelID, supported: /^[CDG]/.test(channelID) }, new WeakSet());
}

process.stdout.write(
  JSON.stringify({
    workspace_id: workspaceId,
    user_id: userId,
    channels,
    members,
    messages,
    provenance: {
      channels: channelProvenance,
      observations: channelObservations,
      messages: messages.map((message) => messageProvenance.get(`${message.channel}|${message.ts}`)),
    },
  })
);
