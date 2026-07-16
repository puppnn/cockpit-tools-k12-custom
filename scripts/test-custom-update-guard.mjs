import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import test from 'node:test';

const readRepositoryFile = (relativePath) =>
  readFileSync(new URL(`../${relativePath}`, import.meta.url), 'utf8');

test('custom builds keep official update installation disabled', () => {
  const customBuildSource = readRepositoryFile('src/utils/customBuild.ts');
  const nativeCustomBuildSource = readRepositoryFile(
    'src-tauri/src/modules/custom_build.rs',
  );
  assert.match(
    customBuildSource,
    /OFFICIAL_UPDATE_INSTALL_ALLOWED\s*=\s*false/,
  );
  assert.match(
    customBuildSource,
    /CUSTOM_BUILD_ID\s*=\s*['"]puppnn\/cockpit-tools-k12-custom['"]/,
  );
  assert.match(
    nativeCustomBuildSource,
    /pub const OFFICIAL_UPDATE_INSTALL_ALLOWED:\s*bool\s*=\s*false/,
  );
});

test('all updater capabilities can check but cannot download or install', () => {
  const capabilityDirectory = new URL('../src-tauri/capabilities/', import.meta.url);
  const capabilityFiles = readdirSync(capabilityDirectory).filter((name) =>
    name.endsWith('.json'),
  );

  assert.ok(capabilityFiles.length > 0);
  for (const capabilityFile of capabilityFiles) {
    const capability = JSON.parse(
      readFileSync(new URL(capabilityFile, capabilityDirectory), 'utf8'),
    );
    const permissions = capability.permissions
      .map((permission) =>
        typeof permission === 'string' ? permission : permission?.identifier,
      )
      .filter((identifier) => typeof identifier === 'string');

    assert.ok(!permissions.includes('updater:default'), capabilityFile);
    assert.ok(
      !permissions.some(
        (permission) =>
          permission.startsWith('updater:allow-') &&
          permission !== 'updater:allow-check',
      ),
      capabilityFile,
    );

    if (capabilityFile === 'default.json') {
      assert.ok(permissions.includes('updater:allow-check'));
      assert.ok(permissions.includes('updater:deny-download'));
      assert.ok(permissions.includes('updater:deny-install'));
      assert.ok(permissions.includes('updater:deny-download-and-install'));
    }
  }
});

test('the Linux installer command checks the native custom-build guard first', () => {
  const updateCommandSource = readRepositoryFile(
    'src-tauri/src/commands/update.rs',
  );
  const guardPosition = updateCommandSource.indexOf(
    'ensure_official_update_install_allowed()?;',
  );
  const installerPosition = updateCommandSource.indexOf(
    'linux_updater::install_linux_update(app, expected_version).await',
  );

  assert.ok(guardPosition >= 0);
  assert.ok(installerPosition >= 0);
  assert.ok(guardPosition < installerPosition);
});
