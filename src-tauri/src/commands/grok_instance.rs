use std::fs;
use std::path::Path;

use serde::Serialize;

use crate::models::{InstanceProfile, InstanceProfileView};
use crate::modules::{self, grok_account, grok_instance};

const DEFAULT_INSTANCE_ID: &str = "__default__";

fn cleanup_legacy_runtime_dir() {
    let Ok(data_dir) = modules::account::get_data_dir() else {
        return;
    };
    let runtime_dir = data_dir.join("grok_runtime");
    if runtime_dir.exists() {
        let _ = fs::remove_dir_all(runtime_dir);
    }
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct GrokInstanceLaunchInfo {
    pub instance_id: String,
    pub user_data_dir: String,
    pub launch_command: String,
}

struct GrokLaunchContext {
    user_data_dir: String,
    working_dir: Option<String>,
    extra_args: String,
    managed: bool,
    /// Official CLI API-key path: export XAI_API_KEY for this launch.
    xai_api_key: Option<String>,
}

fn resolve_launch_account_id(instance_id: &str) -> Result<Option<String>, String> {
    if instance_id == DEFAULT_INSTANCE_ID {
        let settings = grok_instance::load_default_settings()?;
        if settings.follow_local_account {
            return grok_account::current_account_id();
        }
        return Ok(settings.bind_account_id);
    }
    let instance = grok_instance::load_instance_store()?
        .instances
        .into_iter()
        .find(|instance| instance.id == instance_id)
        .ok_or_else(|| "Grok 实例不存在".to_string())?;
    Ok(instance.bind_account_id)
}

fn resolve_xai_api_key_for_instance(instance_id: &str) -> Result<Option<String>, String> {
    let Some(account_id) = resolve_launch_account_id(instance_id)? else {
        return Ok(None);
    };
    let account = grok_account::load_account(&account_id)
        .ok_or_else(|| format!("Grok 账号不存在: {}", account_id))?;
    Ok(account.resolved_api_key().map(|value| value.to_string()))
}

#[cfg(not(target_os = "windows"))]
fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

#[cfg(target_os = "windows")]
fn powershell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "''"))
}

fn normalize_working_dir_override(working_dir: Option<String>) -> Option<String> {
    working_dir
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(|value| value.to_string())
}

fn resolve_context(
    instance_id: &str,
    working_dir_override: Option<Option<String>>,
) -> Result<GrokLaunchContext, String> {
    let xai_api_key = resolve_xai_api_key_for_instance(instance_id)?;
    if instance_id == DEFAULT_INSTANCE_ID {
        let settings = grok_instance::load_default_settings()?;
        return Ok(GrokLaunchContext {
            user_data_dir: grok_instance::get_default_grok_home()?
                .to_string_lossy()
                .to_string(),
            working_dir: match working_dir_override {
                Some(value) => normalize_working_dir_override(value),
                None => settings.working_dir,
            },
            extra_args: settings.extra_args,
            managed: false,
            xai_api_key,
        });
    }

    let instance = grok_instance::load_instance_store()?
        .instances
        .into_iter()
        .find(|instance| instance.id == instance_id)
        .ok_or_else(|| "Grok 实例不存在".to_string())?;
    grok_instance::ensure_managed_instance_path(Path::new(&instance.user_data_dir))?;
    Ok(GrokLaunchContext {
        user_data_dir: instance.user_data_dir,
        working_dir: match working_dir_override {
            Some(value) => normalize_working_dir_override(value),
            None => instance.working_dir,
        },
        extra_args: instance.extra_args,
        managed: true,
        xai_api_key,
    })
}

fn validate_working_dir(context: &GrokLaunchContext) -> Result<Option<&str>, String> {
    let working_dir = context
        .working_dir
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty());
    if let Some(working_dir) = working_dir {
        if !Path::new(working_dir).is_dir() {
            return Err(format!("Grok CLI 工作目录不存在: {}", working_dir));
        }
    }
    Ok(working_dir)
}

fn build_launch_command_with_binary(
    context: &GrokLaunchContext,
    binary: &Path,
) -> Result<String, String> {
    let working_dir = validate_working_dir(context)?;
    let args = modules::process::parse_extra_args(&context.extra_args);

    #[cfg(not(target_os = "windows"))]
    {
        let mut command_parts = Vec::new();
        if let Some(working_dir) = working_dir {
            command_parts.push(format!("cd -- {}", shell_quote(working_dir)));
        }
        let mut command = String::new();
        if let Some(api_key) = context.xai_api_key.as_deref() {
            command.push_str("XAI_API_KEY=");
            command.push_str(&shell_quote(api_key));
            command.push(' ');
        }
        if context.managed {
            command.push_str("GROK_HOME=");
            command.push_str(&shell_quote(&context.user_data_dir));
            command.push(' ');
        }
        command.push_str(&shell_quote(&binary.to_string_lossy()));
        for arg in args {
            let arg = arg.trim();
            if !arg.is_empty() {
                command.push(' ');
                command.push_str(&shell_quote(arg));
            }
        }
        command_parts.push(command);
        return Ok(command_parts.join(" && "));
    }

    #[cfg(target_os = "windows")]
    {
        let mut command_parts = Vec::new();
        if let Some(working_dir) = working_dir {
            command_parts.push(format!(
                "Set-Location -LiteralPath {}",
                powershell_quote(working_dir)
            ));
        }
        if let Some(api_key) = context.xai_api_key.as_deref() {
            command_parts.push(format!(
                "$env:XAI_API_KEY={}",
                powershell_quote(api_key)
            ));
        }
        if context.managed {
            command_parts.push(format!(
                "$env:GROK_HOME={}",
                powershell_quote(&context.user_data_dir)
            ));
        }
        let mut command = format!("& {}", powershell_quote(&binary.to_string_lossy()));
        for arg in args {
            let arg = arg.trim();
            if !arg.is_empty() {
                command.push(' ');
                command.push_str(&powershell_quote(arg));
            }
        }
        command_parts.push(command);
        return Ok(command_parts.join("; "));
    }

    #[allow(unreachable_code)]
    Err("当前系统暂不支持生成 Grok CLI 启动命令".to_string())
}

fn build_launch_command(context: &GrokLaunchContext) -> Result<String, String> {
    let (binary, _) = super::grok::resolve_grok_cli_path()?;
    build_launch_command_with_binary(context, &binary)
}

fn profile_view(mut profile: InstanceProfile) -> InstanceProfileView {
    profile.last_pid = None;
    let initialized = grok_instance::is_profile_initialized(Path::new(&profile.user_data_dir));
    InstanceProfileView::from_profile(profile, false, initialized)
}

fn default_view() -> Result<InstanceProfileView, String> {
    let home = grok_instance::get_default_grok_home()?;
    let settings = grok_instance::load_default_settings()?;
    Ok(InstanceProfileView {
        id: DEFAULT_INSTANCE_ID.to_string(),
        name: String::new(),
        user_data_dir: home.to_string_lossy().to_string(),
        working_dir: settings.working_dir,
        extra_args: settings.extra_args,
        bind_account_id: settings.bind_account_id,
        created_at: 0,
        last_launched_at: None,
        last_pid: None,
        running: false,
        initialized: grok_instance::is_profile_initialized(&home),
        is_default: true,
        follow_local_account: settings.follow_local_account,
    })
}

async fn prepare_bound_account(instance_id: &str) -> Result<(), String> {
    if instance_id == DEFAULT_INSTANCE_ID {
        let settings = grok_instance::load_default_settings()?;
        let account_id = if settings.follow_local_account {
            grok_account::current_account_id()?
        } else {
            settings.bind_account_id
        };
        if let Some(account_id) = account_id {
            grok_account::prepare_account_for_injection(&account_id).await?;
            grok_account::inject_to_default(&account_id)?;
        }
        return Ok(());
    }

    let instance = grok_instance::load_instance_store()?
        .instances
        .into_iter()
        .find(|instance| instance.id == instance_id)
        .ok_or_else(|| "Grok 实例不存在".to_string())?;
    grok_instance::ensure_managed_instance_path(Path::new(&instance.user_data_dir))?;
    if let Some(account_id) = instance.bind_account_id.as_deref() {
        let account = grok_account::prepare_account_for_injection(account_id).await?;
        grok_account::write_account_to_profile(&account, Path::new(&instance.user_data_dir))?;
    }
    Ok(())
}

#[tauri::command]
pub async fn grok_get_instance_defaults() -> Result<modules::instance::InstanceDefaults, String> {
    grok_instance::get_instance_defaults()
}

#[tauri::command]
pub async fn grok_list_instances() -> Result<Vec<InstanceProfileView>, String> {
    cleanup_legacy_runtime_dir();
    let mut views: Vec<_> = grok_instance::load_instance_store()?
        .instances
        .into_iter()
        .map(profile_view)
        .collect();
    views.push(default_view()?);
    Ok(views)
}

#[tauri::command]
pub async fn grok_create_instance(
    name: String,
    user_data_dir: String,
    working_dir: Option<String>,
    extra_args: Option<String>,
    bind_account_id: Option<String>,
    copy_source_instance_id: Option<String>,
    init_mode: Option<String>,
) -> Result<InstanceProfileView, String> {
    let profile = grok_instance::create_instance(grok_instance::CreateInstanceParams {
        name,
        user_data_dir,
        working_dir,
        extra_args: extra_args.unwrap_or_default(),
        bind_account_id,
        copy_source_instance_id,
        init_mode,
    })?;
    Ok(profile_view(profile))
}

#[tauri::command]
pub async fn grok_update_instance(
    instance_id: String,
    name: Option<String>,
    working_dir: Option<String>,
    extra_args: Option<String>,
    bind_account_id: Option<Option<String>>,
    follow_local_account: Option<bool>,
) -> Result<InstanceProfileView, String> {
    let should_sync_account = bind_account_id.is_some() || follow_local_account.is_some();
    if instance_id == DEFAULT_INSTANCE_ID {
        grok_instance::update_default_settings(
            bind_account_id,
            working_dir,
            extra_args,
            follow_local_account,
        )?;
        if should_sync_account {
            prepare_bound_account(DEFAULT_INSTANCE_ID).await?;
        }
        return default_view();
    }
    let profile = grok_instance::update_instance(grok_instance::UpdateInstanceParams {
        instance_id,
        name,
        working_dir,
        extra_args,
        bind_account_id,
    })?;
    if should_sync_account {
        prepare_bound_account(&profile.id).await?;
    }
    Ok(profile_view(profile))
}

#[tauri::command]
pub async fn grok_delete_instance(instance_id: String) -> Result<(), String> {
    if instance_id == DEFAULT_INSTANCE_ID {
        return Err("默认 Grok 实例不可删除".to_string());
    }
    grok_instance::delete_instance(&instance_id)
}

#[tauri::command]
pub async fn grok_start_instance(instance_id: String) -> Result<InstanceProfileView, String> {
    cleanup_legacy_runtime_dir();
    super::grok::resolve_grok_cli_path()?;
    prepare_bound_account(&instance_id).await?;
    if instance_id == DEFAULT_INSTANCE_ID {
        grok_instance::update_default_pid(None)?;
        default_view()
    } else {
        Ok(profile_view(grok_instance::mark_launched(
            &instance_id,
            None,
        )?))
    }
}

#[tauri::command]
pub async fn grok_stop_instance(instance_id: String) -> Result<InstanceProfileView, String> {
    if instance_id == DEFAULT_INSTANCE_ID {
        grok_instance::update_default_pid(None)?;
        default_view()
    } else {
        Ok(profile_view(grok_instance::update_instance_pid(
            &instance_id,
            None,
        )?))
    }
}

#[tauri::command]
pub async fn grok_close_all_instances() -> Result<(), String> {
    let instance_ids = grok_instance::load_instance_store()?
        .instances
        .into_iter()
        .map(|instance| instance.id)
        .collect::<Vec<_>>();
    grok_instance::update_default_pid(None)?;
    for instance_id in instance_ids {
        grok_instance::update_instance_pid(&instance_id, None)?;
    }
    Ok(())
}

#[tauri::command]
pub async fn grok_open_instance_window(_instance_id: String) -> Result<(), String> {
    Err("Grok CLI 不支持窗口定位，请使用“启动”后的命令在终端中运行".to_string())
}

#[tauri::command]
pub async fn grok_get_instance_launch_command(
    instance_id: String,
    working_dir: Option<String>,
    apply_working_dir_override: Option<bool>,
) -> Result<GrokInstanceLaunchInfo, String> {
    let override_value = if apply_working_dir_override.unwrap_or(false) {
        Some(working_dir)
    } else {
        None
    };
    let context = resolve_context(&instance_id, override_value)?;
    Ok(GrokInstanceLaunchInfo {
        instance_id,
        user_data_dir: context.user_data_dir.clone(),
        launch_command: build_launch_command(&context)?,
    })
}

#[tauri::command]
pub async fn grok_execute_instance_launch_command(
    instance_id: String,
    terminal: Option<String>,
    working_dir: Option<String>,
    apply_working_dir_override: Option<bool>,
) -> Result<String, String> {
    let override_value = if apply_working_dir_override.unwrap_or(false) {
        Some(working_dir)
    } else {
        None
    };
    let context = resolve_context(&instance_id, override_value)?;
    let command = build_launch_command(&context)?;
    super::claude::execute_claude_cli_command(&command, terminal)
        .map(|message| message.replace("Claude", "Grok"))
}

#[cfg(test)]
mod tests {
    use super::{build_launch_command_with_binary, GrokLaunchContext};
    use std::path::Path;

    #[test]
    fn default_command_is_direct_and_never_sets_grok_home() {
        let context = GrokLaunchContext {
            user_data_dir: "/tmp/.grok".to_string(),
            working_dir: None,
            extra_args: String::new(),
            managed: false,
            xai_api_key: None,
        };
        let command = build_launch_command_with_binary(&context, Path::new("/opt/grok"))
            .expect("build default command");

        assert!(!command.contains("GROK_HOME"));
        assert!(!command.contains("XAI_API_KEY"));
        assert!(!command.contains("launch-"));
        assert!(!command.contains(".pid"));
        #[cfg(not(target_os = "windows"))]
        assert_eq!(command, "'/opt/grok'");
        #[cfg(target_os = "windows")]
        assert_eq!(command, "& '/opt/grok'");
    }

    #[test]
    fn managed_command_exposes_profile_path_and_arguments() {
        let context = GrokLaunchContext {
            user_data_dir: "/tmp/Grok Home/team's profile".to_string(),
            working_dir: None,
            extra_args: "--label \"team's files\"".to_string(),
            managed: true,
            xai_api_key: None,
        };
        let command = build_launch_command_with_binary(&context, Path::new("/opt/Grok CLI/grok"))
            .expect("build managed command");

        assert_eq!(command.matches("GROK_HOME").count(), 1);
        assert!(!command.contains("launch-"));
        assert!(!command.contains(".pid"));
        assert!(command.contains("team"));
        assert!(command.contains("/opt/Grok CLI/grok"));
    }

    #[test]
    fn api_key_command_exports_xai_api_key() {
        let context = GrokLaunchContext {
            user_data_dir: "/tmp/.grok".to_string(),
            working_dir: None,
            extra_args: String::new(),
            managed: false,
            xai_api_key: Some("xai-test-key".to_string()),
        };
        let command = build_launch_command_with_binary(&context, Path::new("/opt/grok"))
            .expect("build api key command");
        assert!(command.contains("XAI_API_KEY"));
        assert!(command.contains("xai-test-key"));
    }
}
