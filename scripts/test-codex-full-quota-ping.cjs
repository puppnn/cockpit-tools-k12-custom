const assert = require('node:assert/strict');
const fs = require('node:fs');
const Module = require('node:module');
const path = require('node:path');
const ts = require('typescript');

const projectRoot = path.resolve(__dirname, '..');
const sourcePath = path.join(projectRoot, 'src', 'utils', 'codexFullQuotaPing.ts');
const source = fs.readFileSync(sourcePath, 'utf8');
const transpiled = ts.transpileModule(source, {
  fileName: sourcePath,
  reportDiagnostics: true,
  compilerOptions: {
    module: ts.ModuleKind.CommonJS,
    target: ts.ScriptTarget.ES2020,
    esModuleInterop: true,
  },
});

const transpileErrors = (transpiled.diagnostics || []).filter(
  (diagnostic) => diagnostic.category === ts.DiagnosticCategory.Error,
);
assert.equal(
  transpileErrors.length,
  0,
  transpileErrors.map((diagnostic) => ts.flattenDiagnosticMessageText(diagnostic.messageText, '\n')).join('\n'),
);

const loadedModule = new Module(sourcePath, module);
loadedModule.filename = sourcePath;
loadedModule.paths = Module._nodeModulePaths(path.dirname(sourcePath));
loadedModule.require = function requireWithCodexStub(request) {
  if (request === '../types/codex') {
    return {
      isCodexApiKeyAccount(account) {
        return (account.auth_mode || '').trim().toLowerCase() === 'apikey';
      },
    };
  }
  return Module.prototype.require.call(this, request);
};
loadedModule._compile(transpiled.outputText, sourcePath);

const {
  isCodexFullQuotaPingEligible,
  resolveCodexFullQuotaPingTargetWindow,
} = loadedModule.exports;

const NOW_SECONDS = 2_000_000_000;
const FIVE_HOURS_SECONDS = 5 * 60 * 60;
const SEVEN_DAYS_SECONDS = 7 * 24 * 60 * 60;
const THIRTY_DAYS_SECONDS = 30 * 24 * 60 * 60;

function quotaWindow(limitWindowSeconds, resetAfterSeconds = limitWindowSeconds) {
  return {
    used_percent: 0,
    limit_window_seconds: limitWindowSeconds,
    reset_after_seconds: resetAfterSeconds,
    reset_at: NOW_SECONDS + resetAfterSeconds,
  };
}

function makeAccount({ account = {}, quota = {}, rateLimit = {} } = {}) {
  const primaryWindow = quotaWindow(FIVE_HOURS_SECONDS);
  const secondaryWindow = quotaWindow(SEVEN_DAYS_SECONDS);
  const nextRateLimit = {
    primary_window: primaryWindow,
    secondary_window: secondaryWindow,
    ...rateLimit,
  };
  Object.keys(nextRateLimit).forEach((key) => {
    if (nextRateLimit[key] === undefined) delete nextRateLimit[key];
  });

  return {
    id: 'oauth-account',
    email: 'quota-test@example.invalid',
    auth_mode: 'oauth',
    tokens: {
      id_token: 'id-token',
      access_token: 'access-token',
      refresh_token: 'refresh-token',
    },
    requires_reauth: false,
    quota: {
      hourly_percentage: 100,
      hourly_reset_time: NOW_SECONDS + FIVE_HOURS_SECONDS,
      hourly_window_minutes: FIVE_HOURS_SECONDS / 60,
      hourly_window_present: true,
      weekly_percentage: 100,
      weekly_reset_time: NOW_SECONDS + SEVEN_DAYS_SECONDS,
      weekly_window_minutes: SEVEN_DAYS_SECONDS / 60,
      weekly_window_present: true,
      raw_data: { rate_limit: nextRateLimit },
      ...quota,
    },
    ...account,
  };
}

const tests = [
  {
    name: 'selects an unstarted full primary window in the standard 5h and 7d layout',
    run() {
      const account = makeAccount({ quota: { weekly_percentage: 11 } });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
      assert.equal(isCodexFullQuotaPingEligible(account), true);
    },
  },
  {
    name: 'rejects an account when another real window has exactly 10 percent remaining',
    run() {
      const account = makeAccount({ quota: { weekly_percentage: 10 } });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'selects a full unstarted 7d-only secondary window',
    run() {
      const account = makeAccount({
        quota: {
          hourly_window_present: false,
          weekly_window_present: true,
        },
        rateLimit: { primary_window: undefined },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'secondary');
    },
  },
  {
    name: 'selects a full unstarted monthly-only primary window',
    run() {
      const account = makeAccount({
        quota: {
          hourly_window_minutes: THIRTY_DAYS_SECONDS / 60,
          hourly_reset_time: NOW_SECONDS + THIRTY_DAYS_SECONDS,
          hourly_window_present: true,
          weekly_window_present: false,
        },
        rateLimit: {
          primary_window: quotaWindow(THIRTY_DAYS_SECONDS),
          secondary_window: undefined,
        },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
    },
  },
  {
    name: 'selects a full unstarted monthly-only secondary window',
    run() {
      const account = makeAccount({
        quota: {
          hourly_window_present: false,
          weekly_window_minutes: THIRTY_DAYS_SECONDS / 60,
          weekly_reset_time: NOW_SECONDS + THIRTY_DAYS_SECONDS,
          weekly_window_present: true,
        },
        rateLimit: {
          primary_window: undefined,
          secondary_window: quotaWindow(THIRTY_DAYS_SECONDS),
        },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'secondary');
    },
  },
  {
    name: 'rejects a 99 percent window whose countdown has already started',
    run() {
      const account = makeAccount({
        quota: { hourly_percentage: 99, weekly_percentage: 90 },
        rateLimit: { primary_window: quotaWindow(FIVE_HOURS_SECONDS, 1_000) },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'rejects an unstarted target below 99 percent',
    run() {
      const account = makeAccount({ quota: { hourly_percentage: 98, weekly_percentage: 90 } });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'does not treat synthetic 100 percent values as windows when both are absent',
    run() {
      const account = makeAccount({
        quota: {
          hourly_window_present: false,
          weekly_window_present: false,
        },
        rateLimit: {
          primary_window: undefined,
          secondary_window: undefined,
        },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'keeps legacy quota snapshots without presence flags compatible',
    run() {
      const account = makeAccount({ quota: { weekly_percentage: 90 } });
      delete account.quota.hourly_window_present;
      delete account.quota.weekly_window_present;
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
    },
  },
  {
    name: 'uses the raw primary shape when legacy presence flags and raw secondary are absent',
    run() {
      const account = makeAccount({
        quota: { weekly_percentage: 0 },
        rateLimit: { secondary_window: undefined },
      });
      delete account.quota.hourly_window_present;
      delete account.quota.weekly_window_present;
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
    },
  },
  {
    name: 'uses reset_at when reset_after_seconds is unavailable',
    run() {
      const primaryWindow = quotaWindow(FIVE_HOURS_SECONDS);
      delete primaryWindow.reset_after_seconds;
      const account = makeAccount({
        quota: { weekly_percentage: 90 },
        rateLimit: { primary_window: primaryWindow },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
    },
  },
  {
    name: 'treats zero raw and normalized reset timestamps as unavailable',
    run() {
      const primaryWindow = quotaWindow(FIVE_HOURS_SECONDS);
      delete primaryWindow.reset_after_seconds;
      primaryWindow.reset_at = 0;
      const account = makeAccount({
        quota: {
          hourly_reset_time: 0,
          weekly_percentage: 90,
        },
        rateLimit: { primary_window: primaryWindow },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
    },
  },
  {
    name: 'treats reset_after_seconds zero as an expired window ready to restart',
    run() {
      const account = makeAccount({
        quota: { weekly_percentage: 90 },
        rateLimit: { primary_window: quotaWindow(FIVE_HOURS_SECONDS, 0) },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
    },
  },
  {
    name: 'treats a past reset_at as an expired window ready to restart',
    run() {
      const primaryWindow = quotaWindow(FIVE_HOURS_SECONDS);
      delete primaryWindow.reset_after_seconds;
      primaryWindow.reset_at = NOW_SECONDS - 1;
      const account = makeAccount({
        quota: { weekly_percentage: 90 },
        rateLimit: { primary_window: primaryWindow },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'primary');
    },
  },
  {
    name: 'applies the one-second reset tolerance to reset_at fallback',
    run() {
      const primaryWindow = quotaWindow(FIVE_HOURS_SECONDS, FIVE_HOURS_SECONDS - 2);
      delete primaryWindow.reset_after_seconds;
      const account = makeAccount({
        quota: { weekly_percentage: 90 },
        rateLimit: { primary_window: primaryWindow },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'selects another full unstarted window when the first full window is already running',
    run() {
      const account = makeAccount({
        rateLimit: { primary_window: quotaWindow(FIVE_HOURS_SECONDS, 1_000) },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), 'secondary');
    },
  },
  {
    name: 'rejects API key accounts',
    run() {
      const account = makeAccount({ account: { auth_mode: 'apikey' } });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'rejects OAuth accounts without usable tokens',
    run() {
      const account = makeAccount({
        account: { tokens: { id_token: '', access_token: '', refresh_token: '' } },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'rejects OAuth accounts that require reauthentication',
    run() {
      const account = makeAccount({ account: { requires_reauth: true } });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'rejects accounts with a quota error',
    run() {
      const account = makeAccount({
        account: { quota_error: { message: 'quota unavailable', timestamp: NOW_SECONDS } },
      });
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
  {
    name: 'rejects accounts without quota data',
    run() {
      const account = makeAccount();
      delete account.quota;
      assert.equal(resolveCodexFullQuotaPingTargetWindow(account, NOW_SECONDS), null);
    },
  },
];

let failures = 0;
for (const test of tests) {
  try {
    test.run();
    console.log(`ok - ${test.name}`);
  } catch (error) {
    failures += 1;
    console.error(`not ok - ${test.name}`);
    console.error(error);
  }
}

if (failures > 0) {
  console.error(`\n${failures}/${tests.length} full-quota ping tests failed.`);
  process.exitCode = 1;
} else {
  console.log(`\n${tests.length}/${tests.length} full-quota ping tests passed.`);
}
