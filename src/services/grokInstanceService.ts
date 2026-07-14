import { invoke } from '@tauri-apps/api/core';
import { createPlatformInstanceService } from './platform/createPlatformInstanceService';

const service = createPlatformInstanceService('grok');

export const getInstanceDefaults = service.getInstanceDefaults;
export const listInstances = service.listInstances;
export const createInstance = service.createInstance;
export const updateInstance = service.updateInstance;
export const deleteInstance = service.deleteInstance;
export const startInstance = service.startInstance;
export const stopInstance = service.stopInstance;
export const closeAllInstances = service.closeAllInstances;
export const openInstanceWindow = service.openInstanceWindow;

export interface GrokInstanceLaunchInfo {
  instanceId: string;
  userDataDir: string;
  launchCommand: string;
}

export interface GrokCliStatus {
  available: boolean;
  binaryPath?: string | null;
  configuredPath?: string | null;
  version?: string | null;
  source?: string | null;
  message?: string | null;
  checkedAt: number;
}

export async function getGrokCliStatus(): Promise<GrokCliStatus> {
  return await invoke('grok_get_cli_status');
}

export async function updateGrokCliRuntimeConfig(
  grokCliPath?: string | null,
): Promise<GrokCliStatus> {
  return await invoke('grok_update_cli_runtime_config', {
    grokCliPath: grokCliPath?.trim() || null,
  });
}

export async function executeGrokCliInstallCommand(
  terminal?: string,
): Promise<void> {
  await invoke('grok_execute_cli_install_command', {
    terminal: terminal ?? null,
  });
}

export async function getGrokInstanceLaunchCommand(
  instanceId: string,
  options?: {
    workingDir?: string | null;
    applyWorkingDirOverride?: boolean;
  },
): Promise<GrokInstanceLaunchInfo> {
  return await invoke('grok_get_instance_launch_command', {
    instanceId,
    workingDir: options?.workingDir?.trim() || null,
    applyWorkingDirOverride: options?.applyWorkingDirOverride ?? false,
  });
}

export async function executeGrokInstanceLaunchCommand(
  instanceId: string,
  terminal?: string,
  options?: {
    workingDir?: string | null;
    applyWorkingDirOverride?: boolean;
  },
): Promise<string> {
  return await invoke('grok_execute_instance_launch_command', {
    instanceId,
    terminal: terminal ?? null,
    workingDir: options?.workingDir?.trim() || null,
    applyWorkingDirOverride: options?.applyWorkingDirOverride ?? false,
  });
}
