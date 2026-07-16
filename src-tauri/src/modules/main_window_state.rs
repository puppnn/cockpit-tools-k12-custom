//! Persist and restore the main window size/position (#948 / #1132).
//!
//! Independent of general user config so frequent resize writes stay lightweight.
//! Only applies to the `main` window — floating card / OAuth windows are ignored.

use std::path::PathBuf;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tauri::{LogicalPosition, LogicalSize, Manager, Position, Runtime, Size, WebviewWindow, Window};

use crate::modules::{atomic_write, config, logger};

const STATE_FILE: &str = "main_window_state.json";
const MIN_WIDTH: f64 = 900.0;
const MIN_HEIGHT: f64 = 600.0;
const DEFAULT_WIDTH: f64 = 1280.0;
const DEFAULT_HEIGHT: f64 = 800.0;
const SAVE_DEBOUNCE: Duration = Duration::from_millis(250);

static LAST_SAVE_AT: Mutex<Option<Instant>> = Mutex::new(None);

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct MainWindowState {
    pub width: f64,
    pub height: f64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub x: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub y: Option<f64>,
    #[serde(default)]
    pub maximized: bool,
}

impl Default for MainWindowState {
    fn default() -> Self {
        Self {
            width: DEFAULT_WIDTH,
            height: DEFAULT_HEIGHT,
            x: None,
            y: None,
            maximized: false,
        }
    }
}

fn state_path() -> Result<PathBuf, String> {
    let data_dir = config::get_data_dir()?;
    Ok(data_dir.join(STATE_FILE))
}

fn clamp_size(width: f64, height: f64) -> (f64, f64) {
    let w = if width.is_finite() && width > 0.0 {
        width.max(MIN_WIDTH)
    } else {
        DEFAULT_WIDTH
    };
    let h = if height.is_finite() && height > 0.0 {
        height.max(MIN_HEIGHT)
    } else {
        DEFAULT_HEIGHT
    };
    (w, h)
}

pub fn load_main_window_state() -> Option<MainWindowState> {
    let path = state_path().ok()?;
    if !path.exists() {
        return None;
    }
    let content = std::fs::read_to_string(&path).ok()?;
    let mut state: MainWindowState = serde_json::from_str(&content).ok()?;
    let (width, height) = clamp_size(state.width, state.height);
    state.width = width;
    state.height = height;
    if let Some(x) = state.x {
        if !x.is_finite() {
            state.x = None;
        }
    }
    if let Some(y) = state.y {
        if !y.is_finite() {
            state.y = None;
        }
    }
    Some(state)
}

pub fn save_main_window_state(state: &MainWindowState) -> Result<(), String> {
    let (width, height) = clamp_size(state.width, state.height);
    let normalized = MainWindowState {
        width,
        height,
        x: state.x.filter(|v| v.is_finite()),
        y: state.y.filter(|v| v.is_finite()),
        maximized: state.maximized,
    };
    let path = state_path()?;
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).map_err(|e| {
            format!(
                "创建窗口状态目录失败: path={}, error={}",
                parent.display(),
                e
            )
        })?;
    }
    let json = serde_json::to_string_pretty(&normalized)
        .map_err(|e| format!("序列化窗口状态失败: {}", e))?;
    atomic_write::write_string_atomic(&path, &json)
}

/// Capture logical size/position from a live window.
pub fn capture_main_window_state<R: Runtime>(
    window: &WebviewWindow<R>,
) -> Result<MainWindowState, String> {
    let scale = window.scale_factor().unwrap_or(1.0).max(0.1);
    let physical = window
        .inner_size()
        .map_err(|e| format!("读取窗口尺寸失败: {}", e))?;
    let width = physical.width as f64 / scale;
    let height = physical.height as f64 / scale;
    let (width, height) = clamp_size(width, height);

    let maximized = window.is_maximized().unwrap_or(false);
    let (x, y) = if maximized {
        // Keep last known non-maximized position if we already saved one.
        load_main_window_state()
            .map(|s| (s.x, s.y))
            .unwrap_or((None, None))
    } else {
        match window.outer_position() {
            Ok(pos) => {
                let lx = pos.x as f64 / scale;
                let ly = pos.y as f64 / scale;
                (
                    if lx.is_finite() { Some(lx) } else { None },
                    if ly.is_finite() { Some(ly) } else { None },
                )
            }
            Err(_) => (None, None),
        }
    };

    Ok(MainWindowState {
        width,
        height,
        x,
        y,
        maximized,
    })
}

pub fn capture_and_save_main_window<R: Runtime>(window: &WebviewWindow<R>) {
    match capture_main_window_state(window) {
        Ok(state) => {
            if let Err(err) = save_main_window_state(&state) {
                logger::log_warn(&format!("[Window] 保存主窗口尺寸失败: {}", err));
            }
        }
        Err(err) => {
            logger::log_warn(&format!("[Window] 采集主窗口尺寸失败: {}", err));
        }
    }
}

/// Debounced save for continuous resize/move events.
/// Skips mid-drag thrashing; CloseRequested / tray destroy always force-save.
pub fn capture_and_save_main_window_debounced<R: Runtime>(window: &WebviewWindow<R>) {
    {
        let mut last = match LAST_SAVE_AT.lock() {
            Ok(guard) => guard,
            Err(_) => {
                capture_and_save_main_window(window);
                return;
            }
        };
        let now = Instant::now();
        if let Some(prev) = *last {
            if now.duration_since(prev) < SAVE_DEBOUNCE {
                return;
            }
        }
        *last = Some(now);
    }
    capture_and_save_main_window(window);
}

pub fn apply_state_to_window_config(config: &mut tauri::utils::config::WindowConfig) {
    let Some(state) = load_main_window_state() else {
        return;
    };
    config.width = state.width;
    config.height = state.height;
    if let (Some(x), Some(y)) = (state.x, state.y) {
        config.x = Some(x);
        config.y = Some(y);
        config.center = false;
    }
    config.maximized = state.maximized;
}

/// Apply saved geometry to an already-created main window (first launch / recreate).
pub fn restore_to_window<R: Runtime>(window: &WebviewWindow<R>) {
    let Some(state) = load_main_window_state() else {
        return;
    };

    if state.maximized {
        if let Err(err) = window.maximize() {
            logger::log_warn(&format!("[Window] 恢复最大化失败: {}", err));
        }
        return;
    }

    if let Err(err) = window.set_size(Size::Logical(LogicalSize {
        width: state.width,
        height: state.height,
    })) {
        logger::log_warn(&format!("[Window] 恢复窗口尺寸失败: {}", err));
    }

    if let (Some(x), Some(y)) = (state.x, state.y) {
        if let Err(err) = window.set_position(Position::Logical(LogicalPosition { x, y })) {
            logger::log_warn(&format!("[Window] 恢复窗口位置失败: {}", err));
        }
    }
}

/// Helper for events that only give us a Window handle.
pub fn capture_and_save_from_window_handle<R: Runtime>(window: &Window<R>) {
    if window.label() != "main" {
        return;
    }
    let Some(webview) = window.app_handle().get_webview_window("main") else {
        return;
    };
    capture_and_save_main_window(&webview);
}

pub fn capture_and_save_from_window_handle_debounced<R: Runtime>(window: &Window<R>) {
    if window.label() != "main" {
        return;
    }
    let Some(webview) = window.app_handle().get_webview_window("main") else {
        return;
    };
    capture_and_save_main_window_debounced(&webview);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clamp_size_enforces_minimum() {
        let (w, h) = clamp_size(100.0, 50.0);
        assert_eq!(w, MIN_WIDTH);
        assert_eq!(h, MIN_HEIGHT);
    }

    #[test]
    fn clamp_size_keeps_valid() {
        let (w, h) = clamp_size(1400.0, 900.0);
        assert_eq!(w, 1400.0);
        assert_eq!(h, 900.0);
    }
}
