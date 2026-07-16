pub const CUSTOM_BUILD_ID: &str = "puppnn/cockpit-tools-k12-custom";
pub const OFFICIAL_UPDATE_INSTALL_ALLOWED: bool = false;
pub const OFFICIAL_UPDATE_INSTALL_BLOCKED_ERROR_CODE: &str = "custom_build_update_install_blocked";

pub fn ensure_official_update_install_allowed() -> Result<(), String> {
    if OFFICIAL_UPDATE_INSTALL_ALLOWED {
        Ok(())
    } else {
        Err(format!(
            "{}: {} cannot install upstream official binaries",
            OFFICIAL_UPDATE_INSTALL_BLOCKED_ERROR_CODE, CUSTOM_BUILD_ID
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn custom_build_blocks_official_binary_installation() {
        assert_eq!(CUSTOM_BUILD_ID, "puppnn/cockpit-tools-k12-custom");
        let error = ensure_official_update_install_allowed().unwrap_err();
        assert!(error.starts_with(OFFICIAL_UPDATE_INSTALL_BLOCKED_ERROR_CODE));
        assert!(error.contains(CUSTOM_BUILD_ID));
    }
}
