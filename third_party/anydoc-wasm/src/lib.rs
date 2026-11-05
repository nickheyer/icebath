//! Thin C ABI over the anydoc crate for a WebAssembly host without JS glue.
use std::alloc::{alloc, dealloc, Layout};

use anydoc::{ConvertError, Format};

/// Field separator inside an error payload
const SEP: char = '\0';

#[no_mangle]
pub extern "C" fn anydoc_alloc(len: usize) -> *mut u8 {
    if len == 0 {
        return std::ptr::null_mut();
    }
    unsafe { alloc(Layout::from_size_align_unchecked(len, 1)) }
}

#[no_mangle]
pub extern "C" fn anydoc_dealloc(ptr: *mut u8, len: usize) {
    if ptr.is_null() || len == 0 {
        return;
    }
    unsafe { dealloc(ptr, Layout::from_size_align_unchecked(len, 1)) }
}

/// Leaks a string into guest memory and returns ptr and len packed as (ptr << 32) | len
fn leak(s: String) -> u64 {
    let bytes = s.into_boxed_str().into_boxed_bytes();
    let len = bytes.len();
    let ptr = Box::leak(bytes).as_mut_ptr();
    ((ptr as u64) << 32) | (len as u64)
}

/// Variant name, message, and for NeedsOcr the page list and page count, joined by NUL
fn error_payload(err: &ConvertError) -> String {
    let debug = format!("{err:?}");
    let variant: String = debug.chars().take_while(|c| c.is_alphanumeric()).collect();
    let mut payload = format!("{variant}{SEP}{err}");
    if let ConvertError::NeedsOcr { pages, page_count } = err {
        let pages: Vec<String> = pages.iter().map(u32::to_string).collect();
        payload.push(SEP);
        payload.push_str(&pages.join(","));
        payload.push(SEP);
        payload.push_str(&page_count.to_string());
    }
    payload
}

/// Status 0 markdown, 1 error payload, 2 no format from content or name with an empty payload
///
/// ext_is_fallback 0 names the format outright, 1 tries the content first and the name second
#[no_mangle]
pub extern "C" fn anydoc_to_markdown(
    ptr: *const u8,
    len: usize,
    ext_ptr: *const u8,
    ext_len: usize,
    ext_is_fallback: u32,
    status_out: *mut u32,
) -> u64 {
    let data = unsafe { std::slice::from_raw_parts(ptr, len) };
    let ext = if ext_len > 0 {
        unsafe { std::str::from_utf8_unchecked(std::slice::from_raw_parts(ext_ptr, ext_len)) }
    } else {
        ""
    };
    let named = if ext.is_empty() { None } else { Format::from_extension(ext) };
    let format = if ext.is_empty() || ext_is_fallback != 0 {
        Format::from_bytes(data).or(named)
    } else {
        named
    };
    let Some(format) = format else {
        unsafe { *status_out = 2 };
        return leak(String::new());
    };
    match anydoc::to_markdown_bytes(data, format) {
        Ok(text) => {
            unsafe { *status_out = 0 };
            leak(text)
        }
        Err(err) => {
            unsafe { *status_out = 1 };
            leak(error_payload(&err))
        }
    }
}
