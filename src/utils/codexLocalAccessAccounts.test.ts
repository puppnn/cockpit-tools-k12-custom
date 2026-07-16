import assert from "node:assert/strict";
import test from "node:test";
import {
  getCodexLocalAccessAccountIneligibleReason,
  isCodexLocalAccessEligibleAccount,
} from "./codexLocalAccessAccounts.ts";
import type { CodexAccount } from "../types/codex.ts";

function account(partial: Partial<CodexAccount>): CodexAccount {
  return {
    id: partial.id || "acc-1",
    email: partial.email || "a@example.com",
    tokens: partial.tokens || {
      id_token: "",
      access_token: "",
      refresh_token: "",
    },
    created_at: partial.created_at || Date.now(),
    ...partial,
  } as CodexAccount;
}

test("pending oauth accounts are ineligible for API service", () => {
  const pending = account({
    authorization_status: "pending",
    account_note: "import later",
  });
  assert.equal(
    getCodexLocalAccessAccountIneligibleReason(pending, true),
    "pending_oauth",
  );
  assert.equal(isCodexLocalAccessEligibleAccount(pending, true), false);
});

test("authorized oauth accounts remain eligible", () => {
  const ok = account({
    tokens: {
      id_token: "id",
      access_token: "access",
      refresh_token: "refresh",
    },
    plan_type: "plus",
  });
  assert.equal(getCodexLocalAccessAccountIneligibleReason(ok, true), null);
  assert.equal(isCodexLocalAccessEligibleAccount(ok, true), true);
});
