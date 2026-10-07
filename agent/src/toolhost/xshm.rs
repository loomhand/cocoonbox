//! Desktop capture over MIT-SHM: the root window of the display read into one shared segment.

use std::collections::HashMap;
use std::ptr::NonNull;
use std::sync::Mutex;

use x11rb::connection::Connection;
use x11rb::protocol::shape::{ConnectionExt as _, SK};
use x11rb::protocol::shm::ConnectionExt as _;
use x11rb::protocol::xinput::{ConnectionExt as _, DeviceType};
use x11rb::protocol::xproto::{
    Atom, AtomEnum, ConnectionExt as _, EventMask, GrabMode, GrabStatus, ImageFormat, ImageOrder,
    MapState, Window, WindowClass,
};
use x11rb::rust_connection::RustConnection;

use super::cua::SHELL_APPS;

const OVERLAY_NAME: &[u8] = b"Cua.AgentCursorOverlay";
const THUMB_BLOCK: usize = 8;
const PANEL_ROWS: usize = 32;
const DOCK_ROWS: usize = 40;
const ALL_MASTERS: u16 = 1;

/// A mapped child of the root: a window frame, or a popup such as a menu.
#[derive(Clone, PartialEq)]
pub struct Placement {
    pub id: Window,
    pub x: i16,
    pub y: i16,
    pub width: u16,
    pub height: u16,
    /// Override-redirect: a menu, popup or tooltip rather than a managed window.
    pub popup: bool,
    /// InputOutput: drawn on the screen, unlike an InputOnly window.
    pub drawn: bool,
}

impl Placement {
    fn contains(&self, x: i32, y: i32) -> bool {
        let (left, top) = (i32::from(self.x), i32::from(self.y));
        (left..left + i32::from(self.width)).contains(&x)
            && (top..top + i32::from(self.height)).contains(&y)
    }

    /// Whether the screen rectangle `[x, y, w, h]` lies inside this window.
    pub fn holds(&self, [x, y, w, h]: [i32; 4]) -> bool {
        self.contains(x, y) && self.contains(x + w - 1, y + h - 1)
    }
}

/// One X connection and one shared segment the size of the root window.
pub struct Grabber {
    conn: RustConnection,
    root: Window,
    width: u16,
    height: u16,
    seg: u32,
    addr: NonNull<u8>,
    size: usize,
    net_wm_name: Atom,
    utf8: Atom,
    net_active: Atom,
    /// Whether a root child belongs to the desktop shell (panel, dock, desktop), by window id.
    shell: Mutex<HashMap<Window, bool>>,
    net_wm_pid: Atom,
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
        let _ = conn.xinput_xi_query_version(2, 2).map(|c| c.reply());
        let [net_wm_name, utf8, net_active, net_wm_pid] = [
            &b"_NET_WM_NAME"[..],
            b"UTF8_STRING",
            b"_NET_ACTIVE_WINDOW",
            b"_NET_WM_PID",
        ]
        .map(|name| {
            conn.intern_atom(false, name)
                .ok()
                .and_then(|c| c.reply().ok())
                .map_or(u32::from(AtomEnum::NONE), |r| r.atom)
        });
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
            net_wm_name,
            utf8,
            net_active,
            shell: Mutex::new(HashMap::new()),
            net_wm_pid,
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
        let rgb = self.region([0, 0, self.width.into(), self.height.into()])?;
        Ok((u32::from(self.width), u32::from(self.height), rgb))
    }

    /// RGB rows of the screen rectangle `[x, y, w, h]`, which lies inside the screen.
    pub fn region(&self, rect: [i32; 4]) -> Result<Vec<u8>, String> {
        Ok(self
            .image(rect)?
            .as_chunks::<4>()
            .0
            .iter()
            .flat_map(|p| [p[2], p[1], p[0]])
            .collect())
    }

    /// The root window shrunk eightfold by averaging, as RGB rows; the panel and dock strips stay black.
    pub fn thumb(&self) -> Result<Vec<u8>, String> {
        let (w, h) = (usize::from(self.width), usize::from(self.height));
        let (tw, th) = (w / THUMB_BLOCK, h / THUMB_BLOCK);
        let bgrx = self.image([0, 0, self.width.into(), self.height.into()])?;
        let mut sums = vec![0u32; tw * th * 3];
        for y in PANEL_ROWS..(th * THUMB_BLOCK).min(h - DOCK_ROWS) {
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

    /// The mapped children of the root with their geometry: frames, menus and popups, without the desktop shell.
    pub fn mapped(&self) -> Result<Vec<Placement>, String> {
        self.children(false)
    }

    /// The mapped children of the root with their geometry; the desktop shell's only with `shell`.
    fn children(&self, shell: bool) -> Result<Vec<Placement>, String> {
        let tree = self
            .conn
            .query_tree(self.root)
            .map_err(|e| format!("query tree: {e}"))?
            .reply()
            .map_err(|e| format!("query tree: {e}"))?;
        let cookies = tree
            .children
            .iter()
            .map(|&w| {
                let attrs = self
                    .conn
                    .get_window_attributes(w)
                    .map_err(|e| format!("attributes: {e}"))?;
                let geometry = self
                    .conn
                    .get_geometry(w)
                    .map_err(|e| format!("geometry: {e}"))?;
                Ok((w, attrs, geometry))
            })
            .collect::<Result<Vec<_>, String>>()?;
        let mut mapped = Vec::new();
        for (id, attrs, geometry) in cookies {
            let (Ok(attrs), Ok(g)) = (attrs.reply(), geometry.reply()) else {
                continue;
            };
            if attrs.map_state == MapState::VIEWABLE && (shell || !self.is_shell(id)) {
                mapped.push(Placement {
                    id,
                    x: g.x,
                    y: g.y,
                    width: g.width,
                    height: g.height,
                    popup: attrs.override_redirect,
                    drawn: attrs.class == WindowClass::INPUT_OUTPUT,
                });
            }
        }
        self.shell
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .retain(|w, _| tree.children.contains(w));
        Ok(mapped)
    }

    /// The title of window `w`, or of the client inside it when `w` is a window manager frame.
    pub fn title(&self, w: Window) -> String {
        let own = self.name(w);
        if !own.is_empty() {
            return own;
        }
        let children = self
            .conn
            .query_tree(w)
            .ok()
            .and_then(|c| c.reply().ok())
            .map(|t| t.children)
            .unwrap_or_default();
        children
            .iter()
            .map(|&c| self.name(c))
            .find(|n| !n.is_empty())
            .unwrap_or_default()
    }

    /// The window `_NET_ACTIVE_WINDOW` names, the one with keyboard focus.
    pub fn active(&self) -> Option<Window> {
        let reply = self
            .conn
            .get_property(false, self.root, self.net_active, AtomEnum::WINDOW, 0, 1)
            .ok()?
            .reply()
            .ok()?;
        reply.value32()?.next().filter(|w| *w != 0)
    }

    fn is_shell(&self, w: Window) -> bool {
        let mut known = self
            .shell
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        *known.entry(w).or_insert_with(|| {
            let class = self.class(w);
            SHELL_APPS.contains(&class.as_str())
        })
    }

    /// The WM_CLASS class of `w`, such as `Google-chrome`, or of the client inside it when `w` is a window manager frame.
    pub fn class(&self, w: Window) -> String {
        let children = self
            .conn
            .query_tree(w)
            .ok()
            .and_then(|c| c.reply().ok())
            .map(|t| t.children)
            .unwrap_or_default();
        std::iter::once(w)
            .chain(children)
            .find_map(|c| {
                let value = self
                    .conn
                    .get_property(false, c, AtomEnum::WM_CLASS, AtomEnum::STRING, 0, 64)
                    .ok()?
                    .reply()
                    .ok()?
                    .value;
                let class = value.split(|b| *b == 0).nth(1)?;
                (!class.is_empty()).then(|| String::from_utf8_lossy(class).into_owned())
            })
            .unwrap_or_default()
    }

    /// Whether keys sent to the focused window reach `window` of `pid`: it is the active window and holds
    /// the input focus, no popup of another client is on the screen, the keyboard is not grabbed unless
    /// by a popup of `pid`, and the core keyboard is the only master, since Chrome drops core keyboard
    /// input while another one exists.
    pub fn keyboard(&self, window: Window, pid: u32) -> Result<bool, String> {
        if self.active() != Some(window) || self.masters() != Some(1) {
            return Ok(false);
        }
        let focus = self
            .conn
            .get_input_focus()
            .map_err(|e| format!("input focus: {e}"))?
            .reply()
            .map_err(|e| format!("input focus: {e}"))?
            .focus;
        if !self.inside(focus, window)? {
            return Ok(false);
        }
        let (width, height) = (i32::from(self.width), i32::from(self.height));
        let popups: Vec<Option<u32>> = self
            .children(true)?
            .iter()
            .filter(|p| p.popup && p.drawn)
            .filter(|p| {
                let (x, y) = (i32::from(p.x), i32::from(p.y));
                x < width && y < height && x + i32::from(p.width) > 0 && y + i32::from(p.height) > 0
            })
            .map(|p| self.pid(p.id))
            .collect();
        if popups.iter().any(|p| *p != Some(pid)) {
            return Ok(false);
        }
        Ok(!popups.is_empty() || !self.grabbed(false)?)
    }

    /// Whether another client holds an active grab of the keyboard, or of the pointer with `pointer`;
    /// a probe grab of this connection that succeeds is released at once.
    pub fn grabbed(&self, pointer: bool) -> Result<bool, String> {
        let (async_, now) = (GrabMode::ASYNC, x11rb::CURRENT_TIME);
        let status = if pointer {
            let mask = EventMask::NO_EVENT;
            self.conn
                .grab_pointer(false, self.root, mask, async_, async_, 0u32, 0u32, now)
                .map_err(|e| format!("grab pointer: {e}"))?
                .reply()
                .map_err(|e| format!("grab pointer: {e}"))?
                .status
        } else {
            self.conn
                .grab_keyboard(false, self.root, now, async_, async_)
                .map_err(|e| format!("grab keyboard: {e}"))?
                .reply()
                .map_err(|e| format!("grab keyboard: {e}"))?
                .status
        };
        if status == GrabStatus::SUCCESS {
            let released = if pointer {
                self.conn.ungrab_pointer(now)
            } else {
                self.conn.ungrab_keyboard(now)
            };
            released.map_err(|e| format!("ungrab: {e}"))?;
            self.conn.flush().map_err(|e| format!("ungrab: {e}"))?;
        }
        Ok(status != GrabStatus::SUCCESS)
    }

    /// The number of master keyboards.
    fn masters(&self) -> Option<usize> {
        let reply = self
            .conn
            .xinput_xi_query_device(ALL_MASTERS)
            .ok()?
            .reply()
            .ok()?;
        Some(
            reply
                .infos
                .iter()
                .filter(|d| d.type_ == DeviceType::MASTER_KEYBOARD)
                .count(),
        )
    }

    /// The top drawn child of the root at the screen point `(x, y)`.
    pub fn top_at(&self, x: i32, y: i32) -> Result<Option<Placement>, String> {
        Ok(self
            .children(true)?
            .into_iter()
            .rev()
            .find(|p| p.drawn && p.contains(x, y)))
    }

    /// The child of the root that holds window `w`: its window manager frame, or `w` itself.
    pub fn top_level(&self, mut w: Window) -> Result<Window, String> {
        loop {
            let parent = self.parent(w)?;
            if parent == self.root || parent == x11rb::NONE {
                return Ok(w);
            }
            w = parent;
        }
    }

    /// The `_NET_WM_PID` of window `w`.
    pub fn pid(&self, w: Window) -> Option<u32> {
        let reply = self
            .conn
            .get_property(false, w, self.net_wm_pid, AtomEnum::CARDINAL, 0, 1)
            .ok()?
            .reply()
            .ok()?;
        reply.value32()?.next()
    }

    /// Whether `w` is `ancestor` or lies inside it; false for no window and for the pointer root.
    fn inside(&self, mut w: Window, ancestor: Window) -> Result<bool, String> {
        loop {
            if w == ancestor {
                return Ok(true);
            }
            if w == self.root || w <= 1 {
                return Ok(false);
            }
            w = self.parent(w)?;
        }
    }

    fn parent(&self, w: Window) -> Result<Window, String> {
        Ok(self
            .conn
            .query_tree(w)
            .map_err(|e| format!("query tree: {e}"))?
            .reply()
            .map_err(|e| format!("query tree: {e}"))?
            .parent)
    }

    fn name(&self, w: Window) -> String {
        for (atom, kind) in [
            (self.net_wm_name, self.utf8),
            (u32::from(AtomEnum::WM_NAME), u32::from(AtomEnum::STRING)),
        ] {
            let value = self
                .conn
                .get_property(false, w, atom, kind, 0, 64)
                .ok()
                .and_then(|c| c.reply().ok())
                .map(|r| r.value)
                .unwrap_or_default();
            if !value.is_empty() {
                return String::from_utf8_lossy(&value).into_owned();
            }
        }
        String::new()
    }

    fn image(&self, [x, y, w, h]: [i32; 4]) -> Result<&[u8], String> {
        let narrow = |v: i32| i16::try_from(v).map_err(|_| format!("rectangle {x},{y} {w}x{h}"));
        let wide = |v: i32| u16::try_from(v).map_err(|_| format!("rectangle {x},{y} {w}x{h}"));
        let (rx, ry, rw, rh) = (narrow(x)?, narrow(y)?, wide(w)?, wide(h)?);
        self.conn
            .shm_get_image(
                self.root,
                rx,
                ry,
                rw,
                rh,
                !0,
                ImageFormat::Z_PIXMAP.into(),
                self.seg,
                0,
            )
            .map_err(|e| format!("shm get image: {e}"))?
            .reply()
            .map_err(|e| format!("shm get image: {e}"))?;
        let len = usize::from(rw) * usize::from(rh) * 4;
        if len > self.size {
            return Err(format!("rectangle {x},{y} {w}x{h} exceeds the screen"));
        }
        // SAFETY: the reply means the server finished writing `len` bytes; the segment spans `size` bytes for the life of `self`.
        Ok(unsafe { std::slice::from_raw_parts(self.addr.as_ptr(), len) })
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
