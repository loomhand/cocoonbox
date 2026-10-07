//! Desktop capture over MIT-SHM: the root window of the display read into one shared segment.

use std::ptr::NonNull;

use x11rb::connection::Connection;
use x11rb::protocol::shape::{ConnectionExt as _, SK};
use x11rb::protocol::shm::ConnectionExt as _;
use x11rb::protocol::xproto::{
    AtomEnum, ConnectionExt as _, ImageFormat, ImageOrder, MapState, Window,
};
use x11rb::rust_connection::RustConnection;

const OVERLAY_NAME: &[u8] = b"Cua.AgentCursorOverlay";
const THUMB_BLOCK: usize = 8;

/// One X connection and one shared segment the size of the root window.
pub struct Grabber {
    conn: RustConnection,
    root: Window,
    width: u16,
    height: u16,
    seg: u32,
    addr: NonNull<u8>,
    size: usize,
}

// SAFETY: the segment is private to this Grabber and only read through `&self` after a reply.
unsafe impl Send for Grabber {}

impl Grabber {
    /// Connects to `$DISPLAY` and attaches a segment for 32-bit little-endian root pixels.
    pub fn open() -> Result<Self, String> {
        let (conn, n) = x11rb::connect(None).map_err(|e| format!("connect X: {e}"))?;
        let setup = conn.setup();
        let screen = &setup.roots[n];
        let (root, width, height) = (screen.root, screen.width_in_pixels, screen.height_in_pixels);
        let bpp = setup
            .pixmap_formats
            .iter()
            .find(|f| f.depth == screen.root_depth)
            .map(|f| f.bits_per_pixel);
        if screen.root_depth != 24
            || bpp != Some(32)
            || setup.image_byte_order != ImageOrder::LSB_FIRST
        {
            return Err(format!(
                "unsupported root format: depth {} bpp {bpp:?}",
                screen.root_depth
            ));
        }
        conn.shm_query_version()
            .map_err(|e| format!("MIT-SHM: {e}"))?
            .reply()
            .map_err(|e| format!("MIT-SHM: {e}"))?;
        let seg = conn.generate_id().map_err(|e| format!("X id: {e}"))?;
        let size = usize::from(width) * usize::from(height) * 4;
        // SAFETY: a plain syscall that creates a private segment of `size` bytes.
        let id = unsafe { libc::shmget(libc::IPC_PRIVATE, size, libc::IPC_CREAT | 0o600) };
        if id < 0 {
            return Err(format!("shmget: {}", std::io::Error::last_os_error()));
        }
        // SAFETY: `id` is the segment just created; the kernel picks the address.
        let addr = unsafe { libc::shmat(id, std::ptr::null(), 0) };
        let attached = conn
            .shm_attach(seg, u32::try_from(id).unwrap_or_default(), false)
            .map_err(|e| format!("shm attach: {e}"))
            .and_then(|c| c.check().map_err(|e| format!("shm attach: {e}")));
        // SAFETY: marks the segment for removal once both this process and the X server detach.
        unsafe { libc::shmctl(id, libc::IPC_RMID, std::ptr::null_mut()) };
        let addr = NonNull::new(addr.cast::<u8>())
            .filter(|a| a.as_ptr() as isize != -1)
            .ok_or_else(|| format!("shmat: {}", std::io::Error::last_os_error()))?;
        let grabber = Self {
            conn,
            root,
            width,
            height,
            seg,
            addr,
            size,
        };
        attached?;
        Ok(grabber)
    }

    /// Whether a cua-driver cursor overlay is mapped with a visible shape; the driver's capture leaves it out.
    pub fn overlay_shown(&self) -> Result<bool, String> {
        let tree = self
            .conn
            .query_tree(self.root)
            .map_err(|e| format!("query tree: {e}"))?
            .reply()
            .map_err(|e| format!("query tree: {e}"))?;
        let names = tree
            .children
            .iter()
            .map(|&w| {
                let name = self
                    .conn
                    .get_property(false, w, AtomEnum::WM_NAME, AtomEnum::STRING, 0, 16)
                    .map_err(|e| format!("get WM_NAME: {e}"))?;
                Ok((w, name))
            })
            .collect::<Result<Vec<_>, String>>()?;
        for (w, name) in names {
            let name = name.reply().map_err(|e| format!("get WM_NAME: {e}"))?;
            if !name.value.starts_with(OVERLAY_NAME) {
                continue;
            }
            let attrs = self
                .conn
                .get_window_attributes(w)
                .map_err(|e| format!("window attributes: {e}"))?
                .reply()
                .map_err(|e| format!("window attributes: {e}"))?;
            if attrs.map_state != MapState::VIEWABLE {
                continue;
            }
            let shape = self
                .conn
                .shape_get_rectangles(w, SK::BOUNDING)
                .map_err(|e| format!("shape: {e}"))?
                .reply()
                .map_err(|e| format!("shape: {e}"))?;
            if shape.rectangles.iter().any(|r| r.width > 0 && r.height > 0) {
                return Ok(true);
            }
        }
        Ok(false)
    }

    /// The root window as `(width, height, RGB rows)`.
    pub fn rgb(&self) -> Result<(u32, u32, Vec<u8>), String> {
        let rgb = self
            .bgrx()?
            .as_chunks::<4>()
            .0
            .iter()
            .flat_map(|p| [p[2], p[1], p[0]])
            .collect();
        Ok((u32::from(self.width), u32::from(self.height), rgb))
    }

    /// The root window shrunk eightfold by averaging, as RGB rows.
    pub fn thumb(&self) -> Result<Vec<u8>, String> {
        let (w, h) = (usize::from(self.width), usize::from(self.height));
        let (tw, th) = (w / THUMB_BLOCK, h / THUMB_BLOCK);
        let bgrx = self.bgrx()?;
        let mut sums = vec![0u32; tw * th * 3];
        for y in 0..th * THUMB_BLOCK {
            let row = &bgrx[y * w * 4..(y * w + tw * THUMB_BLOCK) * 4];
            let base = y / THUMB_BLOCK * tw * 3;
            for (x, p) in row.as_chunks::<4>().0.iter().enumerate() {
                let i = base + x / THUMB_BLOCK * 3;
                sums[i] += u32::from(p[2]);
                sums[i + 1] += u32::from(p[1]);
                sums[i + 2] += u32::from(p[0]);
            }
        }
        let n = u32::try_from(THUMB_BLOCK * THUMB_BLOCK).unwrap_or(u32::MAX);
        Ok(sums
            .iter()
            .map(|s| u8::try_from(s / n).unwrap_or(u8::MAX))
            .collect())
    }

    fn bgrx(&self) -> Result<&[u8], String> {
        let (w, h) = (self.width, self.height);
        self.conn
            .shm_get_image(
                self.root,
                0,
                0,
                w,
                h,
                !0,
                ImageFormat::Z_PIXMAP.into(),
                self.seg,
                0,
            )
            .map_err(|e| format!("shm get image: {e}"))?
            .reply()
            .map_err(|e| format!("shm get image: {e}"))?;
        // SAFETY: the reply means the server finished writing; the segment spans `size` bytes for the life of `self`.
        Ok(unsafe { std::slice::from_raw_parts(self.addr.as_ptr(), self.size) })
    }
}

impl Drop for Grabber {
    fn drop(&mut self) {
        let _ = self.conn.shm_detach(self.seg);
        let _ = self.conn.flush();
        // SAFETY: `addr` came from shmat and is not used after this.
        unsafe { libc::shmdt(self.addr.as_ptr().cast()) };
    }
}
