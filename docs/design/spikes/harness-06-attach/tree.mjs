#!/usr/bin/env node
// Spike 06: read a transcript .jsonl as the tree its parentUuid links make.
//
// Usage: node tree.mjs <transcript.jsonl>
//
// Prints each conversational leaf (a user/assistant record with no
// user/assistant descendant) and its chain of user/assistant text back to the
// root. Two processes appending to one file show up as two leaves whose chains
// share a prefix: a fork inside one file. Lines that are not valid JSON (a torn
// interleaved write) are counted. Node builtins only.
import { readFileSync } from "node:fs";

const recs = [];
let bad = 0;
for (const line of readFileSync(process.argv[2], "utf8").split("\n")) {
  if (!line.trim()) continue;
  try { recs.push(JSON.parse(line)); } catch { bad++; }
}
const by = new Map(recs.filter((r) => r && r.uuid).map((r) => [r.uuid, r]));
const kids = new Map();
for (const r of by.values()) {
  const p = r.parentUuid ?? null;
  if (!kids.has(p)) kids.set(p, []);
  kids.get(p).push(r.uuid);
}
const isConv = (u) => ["user", "assistant"].includes(by.get(u)?.type);
const convBelow = (u) => {
  const stack = [...(kids.get(u) ?? [])];
  while (stack.length) {
    const c = stack.pop();
    if (isConv(c)) return true;
    stack.push(...(kids.get(c) ?? []));
  }
  return false;
};
const text = (r) => {
  const c = r.message?.content;
  if (typeof c === "string") return c;
  if (Array.isArray(c)) return c.map((x) => x.text ?? x.type ?? "?").join("|");
  return "";
};
const leaves = [...by.keys()].filter((u) => isConv(u) && !convBelow(u))
  .sort((a, b) => String(by.get(a).timestamp).localeCompare(String(by.get(b).timestamp)));
const sessions = [...new Set(recs.map((r) => r?.sessionId).filter(Boolean))];
console.log(`records=${recs.length} invalid_lines=${bad} sessionIds=${sessions.join(",")}`);
console.log(`conversation leaves=${leaves.length} (${leaves.length === 1 ? "one chain" : "FORKED inside one file"})`);
for (const leaf of leaves) {
  const chain = [];
  for (let u = leaf; by.has(u); u = by.get(u).parentUuid) {
    const r = by.get(u);
    const t = isConv(u) ? text(r).replace(/\s+/g, " ") : "";
    if (t && !t.startsWith("<") && !["tool_use", "tool_result", "thinking"].includes(t)) {
      chain.push(`${r.type[0]}:${t.slice(0, 50)}`);
    }
  }
  console.log(`  leaf ${leaf.slice(0, 8)}: ${chain.reverse().join(" -> ")}`);
}
