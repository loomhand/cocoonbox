# Bench host measurements

Measurements and lessons from the bench host (GCP c3-standard-22-lssd).

## Clone timing

Measured on 2026-09-30 on the cocoon-cb stack: provisioning about 30 s on a warm apt cache; snapshot save 48–64 s (13 GB, and ext4 has no reflink, so each clone copies its disk in full); clone to Ready 37 s for v1 (probe 22) and 20 s for v2 (probe 7443); hibernate 144–154 s (uncompressed push); wake 18–19 s.

History: the cocoon-cb store and CocoonSet clones, removed 2026-10-01. Measured 2026-09-30 over four v2 clones with a host tracer (1 s samples of the new CH process and of `nvme0n1`), in-guest timings through `vm exec`, and a page-cache residency probe.

Storage: snapshot `cocoonbox-desktop-v2` keeps `memory-ranges` (8.0 GiB) and `cow.raw` (40 GiB sparse, 4.4 GiB used). That is on `/` (ext4) on `nvme0n1`, the GCP persistent disk. The two unused local NVMe drives are not used for this.

Quiet host (run 4, Ready at +23 s):

| t (s) | Event | Evidence |
|---|---|---|
| 0–6 | `cocoon vm clone` copies `cow.raw` in full, because ext4 has no reflink | 837 MiB read from the disk; host Dirty+Writeback rises to 4.5 GiB |
| 7 | CH runs; vk starts the post-clone `vm exec` | vk log `post-clone setup starting` |
| 7–22 | the guest waits on disk I/O while the host flushes the copy | CH is flat: minflt 28.3k, majflt 25, read_bytes 116 MB, RSS 504 MB, ~3 CPU ticks/s. Host writeback drains at ~290 MB/s. Guest `drop_caches` takes 14,670 ms and guest io pressure goes from avg10 11 to 76 |
| 22 | host writeback reaches 11 MiB; both drop_caches calls (vk's and the probe's) finish in the same second | guest kernel `drop_caches: 3` at 08:42:13.17 |
| 22–23 | the rest of the script and readiness | networkd restart ~20 ms, cold read 16 ms, sync 101 ms |

Guest memory page-in is not the cost. `memory-ranges` is mostly in the host page cache, and the guest touches about 0.5 GiB of it. `vm exec` over vsock works 0.66 s after restore (strace: `CONNECT 1024` answered in 54 ms). The stall comes from direct I/O: CH opens writable disks with O_DIRECT, and direct I/O on a file whose page cache is still dirty waits until that range is written back.

Under other host writes (run 3, while another box was hibernating, with host Dirty already at 7.4 GiB), the copy itself was throttled: 78 s from CreatePod to CH start, and 91 s to Ready.

Run 5 (quiet host, the requested single instrumented clone). The memory file is the snapshot's `memory-ranges`, 8 GiB, on `/dev/nvme0n1p1` (ext4, `nvme_card-pd`), 36% resident in the host page cache at start. CH majflt stays at 0, read_bytes at 15.7 MB and RSS at 574 MB. Post-clone ran from 08:47:19.59 to 08:47:35.48 (15.9 s). Guest `drop_caches` finished at 35.40 and networkd reported `Started` at 35.48, while host dirty+writeback went from 4.3 GiB to 1 MiB over the same window. Run warm right after Ready, the same three commands take 0.010 s (`drop_caches`), 0.004 s (rewrite the networkd file) and 0.137 s (`systemctl restart systemd-networkd`).

### After the move (2026-09-30, 10:04Z)

| Metric | Persistent disk (ext4) | Local NVMe (XFS reflink) |
|---|---|---|
| Disk use per clone (`df` delta on `/srv/cb`) | ~4.4 GiB copy | 4 MiB and 5 MiB |
| `cow.raw` extents | private copy | 3913 extents with `0x2000` shared (`xfs_io -c 'fiemap -v'`), `filefrag -v` reports `shared` |
| Clone to Ready, quiet host | 20–37 s | 10.1 s cold (memory file not yet in page cache: CH read 2.4 GB, majflt 216), 3.9 s warm |
| vk `post-clone … attempt_dur` | 15.9–16.3 s | 4.45 s cold, 2.39 s warm |
| Hibernate (suspend to `Suspended`) | 144–159 s | 80 s |
| Wake to Ready | 18–19 s | 4.6 s (dev-agent and smoke) |

The marker `/etc/cocoonbox/nvme-move-marker` survived hibernate, move and wake on dev-agent, and hibernate and wake on smoke.

Phase timings: hibernate both boxes 60 s + 159 s (serial), stop 4 s, re-check + mkfs + mount 3 s, rsync of 49 GB allocated (159 GB apparent) 287 s, verify (248-entry tree diff, `cmp` of meta.db and the v2 memory file) about 30 s, swap + fstab + quota + `VK_NODE_STORAGE=320Gi` 1 s, restart 32 s, wake dev-agent 4.6 s. The two boxes were unavailable for about 12 min.

## VNC server: x11vnc (v2) vs TigerVNC (v3)

Measured on 2026-09-30 on the golden (clone of v2), real Xorg `:0` with the dummy driver at 1280x800. The client is an RFB benchmark script on the host, through `ssh -L` to the guest's `127.0.0.1:5900` (Raw encoding, 32 bpp). It answers fences and runs notify/request/provide for the extended clipboard. The driver script runs 45 s idle, a first connect, 20 s of static desktop, then 20 s full-frame and 20 s incremental against a fullscreen `xfce4-terminal` streaming base64. CPU comes from sampling `/proc` in the guest, where 100% is one vCPU.

| | x11vnc 0.9.16 (v2) | TigerVNC Xorg module 1.13.1 (chosen) | x0vncserver 1.13.1 |
|---|---|---|---|
| connect → banner (after 45 s idle) | 151–212 ms | 49–53 ms | 52 ms |
| connect → first full update (request → update) | 1226–1290 ms (1030–1133 ms) | 65–72 ms (13–15 ms) | 86 ms (23 ms) |
| full-frame Raw, static desktop | 25–27 fps, 98–104 MiB/s, p99 102–194 ms, max ~520 ms | 75.9 fps, 296.5 MiB/s, p99 53 ms, max 100 ms | 75.3 fps, 294.3 MiB/s, p99 54 ms, max 100 ms |
| full-frame Raw, animation | 20.9 fps, 81.8 MiB/s | 23.7 fps, 92.6 MiB/s | 27.8 fps, 108.7 MiB/s |
| incremental, animation | 18.8 upd/s, 42.6 MiB/s, max gap 164 ms | 30.1 upd/s, 66.4 MiB/s, max gap 138 ms | 31.2 upd/s, 70.2 MiB/s, max gap 151 ms |
| gaps > 1 s (all runs) | 0 | 0 | 0 |
| ContinuousUpdates (-313) / Fence (-312) | no / no | yes / yes | yes / yes |
| ExtendedClipboard UTF-8 | caps offered (0x17000001), but the server **closes the connection** on an extended provide | works both ways: guest `xclip -o` reads `héllo 世界 ✓` sent by the client; client receives `guést 世界 ✓` set in the guest | no clipboard in 1.13: guest sees no owner, client receives nothing |
| CPU idle, no client | 0% | 0% (Xorg) | 0% |
| CPU during animation, one client | x11vnc 15% (+ Xorg ~13%); guest 48% of 4 vCPUs | Xorg 62% (includes the ~13% Xorg spends drawing without VNC); guest 53% | X0tigervnc 49% (+ Xorg ~13%); guest 54% |
| CPU during animation, no client | 0% | Xorg 17–18% (~13% without the module) | 0% |

Both TigerVNC variants are about 3x x11vnc on raw throughput and 15–20x faster to the first update. The module is chosen because it has the only working UTF-8 clipboard, needs no extra process, and gets damage straight from the X server. Its cost is about 4–5% of a vCPU in Xorg while the screen changes and nobody is connected. The ~4.5 s x11vnc stall seen earlier through the gRPC relay did not reproduce with this client over `ssh -L`. x11vnc's slow first update after idle (about 1.1 s from request to update) did reproduce.

cua-driver regression on the module:
- `doctor` is clean.
- An element-token click on Chrome turns "none clicked" into "A clicked" while Mousepad stays focused (0x1c00003 before and after).
- A background pixel click (`path=mpx_pointer`, focus restored) plus background `type_text` (`path=mpx_uinput`) produce `line twoV3` in Mousepad while Chrome keeps focus (0x1e00002).
- `end_session` returns the master devices to 1.

v3 smoke clone (probePort 7443) on NVMe:
- Ready in 4.5 s (3.3 s after the recut with agent ca812fb; `sha256sum /usr/local/bin/cocoonbox-agent` in the clone = `55511ae3…`).
- `Session/Hello` answers, and `Session/Screen` returns the `RFB 003.008\n` banner through the agent relay.
- Through `ssh -L`: first full update 163 ms, full-frame Raw 76.5 fps.

## Instrumented hibernate (2026-10-02)

- One throwaway root claim of `cbdesk:v15` (a warm clone of the b239446 golden), idle 180 s, then hibernate, wake and release (bpftrace `hib.bt`: the cocoon CLI's CH API requests and responses, its opens under the snapshot store, every fsync with its duration, renames, FICLONE; CH's own large writes aggregated). The first run (`run1`, hibernate 28.4 s) went untraced because bpftrace hit the BPF stack limit; `run2` is the traced one. Kernel `7.0.0-1011-gcp`; the snapshot store, the golden's `memory-ranges` and the claim's run dir are all XFS on `/dev/nvme3n1` (st_dev 259:2), so the capture moves into the store by rename.
- Before the hibernate: CH `vm.info` memory 8 GiB, balloon 2 GiB, `memory_actual_size` 6 GiB (settled). smaps of the CH process: the guest RAM is two `rw-p` MAP_PRIVATE VMAs of the run dir's `memory-ranges` (5 GiB + 3 GiB, a hard link to the golden's), Rss 1.58 GB, Shared_Clean 1.36 GB, Private_Dirty 216.9 MB = Anonymous 216.9 MB; the whole process has Private_Dirty 218.6 MB and Anonymous 218.9 MB, so about 1.7 MB lies outside guest RAM (run1: 250.1 MB in the guest region).
- Hibernate 22.73 s wall (API call to response), from the trace (offsets from the call): `vm.info` +0.012 (balloon already settled, so no wait), `vm.pause` +0.013 to +0.015, `vm.snapshot` +0.015 to +2.027 (2.01 s; CH's large writes add up to 2.01 s in the syscall, so the 8 GiB dump lands in the page cache), FICLONE of `cow.raw` +2.028, then cocoon's own fsyncs before the store move: 0.49 s (the atomic `cocoon.json` write) and 0.93 s, 0.96 s and 2.74 s on its fd 9 (likely the sqlite meta store), rename into the store +7.158, then SyncTree +7.158 to +22.597 (15.44 s: `config.json` 0.13 s, `cow.raw` 0.08 s, `memory-ranges` 15.23 s, the rest under 1 ms), `vm.shutdown` +22.598 to +22.729. So about 15.2 s is the writeback of the 8 GiB memory file (about 0.53 GiB/s) and about 5 s is small fsyncs queued behind it; the dump itself is 2 s.
- Snapshot dir before the wake: 51,539,706,795 B apparent, 8,620,789,760 B allocated (`memory-ranges` 8.0 GiB fully allocated although only about 217 MB of guest RAM was private-dirty; `cow.raw` 40 GiB apparent, 30 MiB allocated by the reflink). Wake 0.094 s. CH logs nothing at its default level (an empty `cloud-hypervisor.log`), so the CH-side times come from the trace. Both claims released; the snapshot store is empty again.

## Guest RAM page map of an idle warm clone (2026-10-02)

- One throwaway root claim of `cbdesk:v15` (warm clone `sbx-1b74d123…-bb5b1f537a81` of the b239446 golden, created 07:47Z), idle 180 s after the claim, no hibernate. `/proc/<ch pid>/pagemap` read over the two guest RAM VMAs only (the `rw-p` MAP_PRIVATE `memory-ranges` mappings, 5 GiB + 3 GiB, 2,097,152 pages). A page counts as changed when present with bit 61 clear or swapped (bit 62), unchanged when not present or present with bit 61 set; runs do not cross the two VMAs.
- Changed: 58,125 pages = 238,080,000 B, exactly the guest VMAs' smaps `Private_Dirty` (and `Anonymous`) at the same moment; none swapped. Unchanged: 333,925 present file pages (the golden's page cache) and 1,705,102 not present.
- 7,160 changed runs: p50 1, p90 5, p99 32, max 9,728 pages; 4,215 are single pages, 6,804 are at most 8 pages, 10 are 512 pages or longer. 7,160 unchanged runs: p50 4, p90 39, p99 2,307, max 717,820 pages; 215 are 512 pages or longer.
- Soft-dirty: `clear_refs 4` on that CH only after the 180 s idle, then 60 s: 6,530 pages soft-dirty, of which 6,159 were already changed (redirtied) and 371 newly changed; 51,966 changed pages were not written again in the window; no changed page went back. Released afterwards.

## Experimental CH reflink snapshot (2026-10-02)

- Build: cocoonstack/cloud-hypervisor fork `dev` 8ff614177 from the unmerged branch `cow-snapshot-reflink` (e1f32599c), sha256 `497ebc516f9c1483c70ceb30bbcbb9fb960b07496fe7dc3ae62e20bcef7219d4`, `v54.0.0`, features `fw_cfg`, `io_uring`, `kvm`. On a full snapshot of a VM restored copy-on-write, it FICLONEs the restore source into `memory-ranges` and writes only the changed pages (pagemap, gaps of up to 8 pages merged).
- Test (bpftrace `hib2.bt`): two throwaway `cbdesk:v16` claims on the new build (the warm VM from before the swap was claimed and released first, since it still ran the old binary). A idled 180 s (Private_Dirty 227 MB), got a 64 MiB random file in its `/dev/shm` (tmpfs), then a traced hibernate; B ran a guest loop that faults in 4 MiB of fresh memory about every 28 ms (2.5 GiB in all) across A's hibernate.
- Hibernate 1, 1.03 s instead of 22.73 s: `vm.snapshot` 0.91 s (+0.015 to +0.929); inside it CH's FICLONE of the restore source took 87 µs (+0.032) and its page writes 274.5 MB in 2,549 calls, 0.21 s in the syscall (+0.017 to +0.244); CH's `/proc/<pid>/io` `write_bytes` rose by 274,497,536, so the 8 GiB were not rewritten. The rest of the 0.91 s is not attributed by this trace. Then cocoon's FICLONE of `cow.raw` 24 ms and every fsync under 8 ms; SyncTree's `memory-ranges` fsync 27 µs (15.23 s before); `vm.shutdown` 0.06 s. `memory-ranges`: 8 GiB apparent, 5,096 extents, 2,548 of them shared (`filefrag -v`; `du` counts shared blocks, so it still reports 8.2 GiB).
- B during it: its 4 MiB fault-ins took 18 ms (median) before; at the FICLONE (+0.032) 20 ms; then 53, 36, 32 and 24 ms from +0.086 to +0.24, while A's page writes ran, and normal again from +0.30. So the 87 µs FICLONE did not stall a sibling clone of the same golden; the page writes slowed it briefly (guest clock offset −29 ms, measured with a 20 ms round trip).
- Correctness: the hibernate snapshot exported with `snapshot export --to-dir` (0.12 s, a reflink copy) and cloned as a separate VM showed the same sha256 for the tmpfs file; the wake (0.45 s) showed the same sha256 with xorg, session and toolhost active.
- Hibernate 2 (the woken VM, idle 60 s more): 0.30 s; the FICLONE of the restore source, now the gen-1 file, took 101 ms, page writes 37.9 MB in 1,504 calls; `memory-ranges` 7,015 extents, 5,512 shared. The clone path still applies. Wake 0.66 s, same sha256, desktop units active.
- Not captured: CH's info line (cocoon starts CH without `-v`; the build's path shows in the trace and in `write_bytes` instead), and a sibling's latency during the 101 ms gen-2 FICLONE (B's loop had ended). The snapshot dirs were removed by the wakes, so the extent lengths were not kept.

## CH reflink snapshot, 10-generation chain (2026-10-02)

- Build: the pinned 8ff614177 (`497ebc51…`). One throwaway `cbdesk:v16` claim on the new build (bpftrace `hib3.bt`): idle 180 s, a 64 MiB random file in its tmpfs, then hibernate → wake → sha256 → 60 s idle, ten times.

| gen | source extents (shared) | FICLONE | CH written / calls | vm.snapshot | hibernate | new extents (shared) | shared blocks | wake |
|---|---|---|---|---|---|---|---|---|
| 1 | 2 (0) | 0.08 ms | 264.8 MB / 2,527 | 0.716 s | 0.834 s | 5,052 (2,526) | 96.9% | 0.485 s |
| 2 | 5,052 (2,526) | 91.9 ms | 32.0 MB / 1,419 | 0.210 s | 0.300 s | 6,848 (5,430) | 99.6% | 0.705 s |
| 3 | 6,848 (2,658) | 127.5 ms | 33.6 MB / 1,411 | 0.251 s | 0.326 s | 7,392 (5,982) | 99.6% | 0.513 s |
| 4 | 7,392 (2,731) | 137.4 ms | 33.0 MB / 1,403 | 0.256 s | 0.340 s | 7,654 (6,252) | 99.6% | 0.701 s |
| 5 | 7,654 (2,741) | 135.5 ms | 32.3 MB / 1,394 | 0.254 s | 0.337 s | 7,833 (6,440) | 99.6% | 0.822 s |
| 6 | 7,833 (2,749) | 137.8 ms | 34.2 MB / 1,420 | 0.271 s | 0.353 s | 7,902 (6,483) | 99.6% | 0.663 s |
| 7 | 7,902 (2,755) | 139.6 ms | 59.6 MB / 1,912 | 0.327 s | 0.416 s | 7,980 (6,069) | 99.3% | 0.484 s |
| 8 | 7,980 (2,824) | 147.5 ms | 32.3 MB / 1,398 | 0.261 s | 0.343 s | 8,581 (7,184) | 99.6% | 0.576 s |
| 9 | 8,581 (2,829) | 150.8 ms | 35.6 MB / 1,432 | 0.275 s | 0.359 s | 8,628 (7,197) | 99.6% | 0.693 s |
| 10 | 8,628 (2,834) | 153.4 ms | 32.5 MB / 1,422 | 0.273 s | 0.355 s | 8,840 (7,419) | 99.6% | 0.661 s |

- "CH written" is the traced write bytes; `/proc/<ch>/io` `write_bytes` agreed within 6 KiB every generation. Extents and shared blocks are `filefrag -v` of the 8 GiB `memory-ranges` (2,097,152 blocks). The source of generation n is the run dir's restore file, which shares fewer extents than the snapshot it came from because the wake deleted that snapshot. FICLONE time grows with the source's extent count (about 18 µs per extent from gen 2 on), and the count grows by +47 to +601 per generation after gen 3. The sha256 of the tmpfs file matched at every wake, gen 10 included, and xorg, session and toolhost were active at the end.
- vm.snapshot's tail: in every generation the reply came right after CH's `close` of the memory file returned; that close spent its time in `xfs_file_release` → `filemap_flush` (626 ms in gen 1, 85 to 147 ms after), and the block requests issued in CH's context (2,579 requests, 264.7 MB in gen 1; about 32 MB later) cover exactly the bytes CH wrote. So the tail is XFS flushing the file on close (release after a size change), not CH work.

## Wake A/B: dense vs reflink snapshots, hot vs cold page cache (2026-10-02)

- Why wakes are slower on the reflink build (0.45–0.82 s against 0.09–0.12 s). All cases on throwaway `cbdesk:v17` clones of one golden (the default pool by then), idle 180 s (the second clone of a pair waits one case longer), hibernated and woken by the same CH: dense on 6f08b0859, reflink on 8ff614177. Cold means `sync; echo 1 > /proc/sys/vm/drop_caches` after the hibernate and 2 s before the wake. "Wake" is the sandboxd `POST /wake` call alone; reads are the host-wide `nvme3n1` `/proc/diskstats` delta over that call; faults are the new CH's from `/proc/<pid>/stat` at the first guest exec. THP `enabled` and `defrag` are `madvise`.

| case | snapshot `memory-ranges` extents (shared) | wake | read over the wake | to first exec | majflt / minflt |
|---|---|---|---|---|---|
| dense-hot | 2 (0) | 0.146 s | 17 reads, 0.6 MB | 0.213 s | 1 / 4,804 |
| dense-hot | 2 (0) | 0.145 s | 923 reads, 5.2 MB | 0.215 s | 0 / 4,021 |
| dense-cold | 2 (0) | 1.036 s | 2,979 reads, 732.9 MB | 1.284 s | 69 / 3,288 |
| dense-cold | 2 (0) | 1.124 s | 961 reads, 774.9 MB | 1.258 s | 59 / 2,927 |
| reflink-hot | 4,775 (2,388) | 0.444 s | 3,316 reads, 292.3 MB | 0.527 s | 27 / 3,539 |
| reflink-hot | 4,919 (2,460) | 0.432 s | 3,569 reads, 280.9 MB | 0.495 s | 30 / 3,734 |
| reflink-cold | 5,149 (2,575) | 0.908 s | 5,450 reads, 637.3 MB | 1.195 s | 68 / 3,001 |
| reflink-cold | 4,975 (2,488) | 0.951 s | 5,825 reads, 659.3 MB | 1.101 s | 63 / 2,803 |
| reflink-cold, gen 5 | 7,242 (6,247) | 0.862 s | 6,529 reads, 581.5 MB | 1.013 s | 63 / 2,564 |

- A restore reads about 0.6–0.8 GB of the memory file. A dense snapshot just written by CH sits whole in the page cache, so its hot wake reads almost nothing. A reflink snapshot's page cache holds only the pages CH wrote (about 270 MB); its shared blocks are cached, if at all, under the golden's inode, so even its "hot" wake reads about 0.29 GB from disk and takes 0.43–0.44 s. After a drop both kinds read 0.6–0.8 GB and take 0.86–1.12 s. The page-cache explanation holds. The gen-5 file (7,242 extents) woke no slower cold than the gen-1 files (0.862 s against 0.908 and 0.951 s). Every case kept its 16 MiB tmpfs sha256; chain gens 2–4 took 0.75–0.80 s per hibernate and wake.
- Hibernates on 6f08b0859 took 22.7–32.1 s here (the host was busier), on 8ff614177 0.30–0.90 s.

## CH f1dcd9900: the reworked reflink snapshot (2026-10-05)

- Build: cocoonstack/cloud-hypervisor fork `dev` f1dcd9900 (`build-info.json` commit f1dcd9900059…, run 37256978682, rustc 1.99.0, `v54.0.0`, features `fw_cfg`, `io_uring`, `kvm`), sha256 `11eae7807db48c3e7641d1142af4a7a8b8a2dd659c8e96d02ab45200457f9d33` (`SHA256SUMS` agrees). Its fork commit f1dcd99000 opens `/proc/self/pagemap` once at the copy-on-write restore and keeps the fd, reads no `/proc/self/maps`, sorts the snapshot ranges by guest address and allows `FICLONE` in the vmm thread's seccomp filter; the snapshot format is unchanged. `dev` was also rebuilt on newer upstream (compare 1413535b8...f1dcd9900: 12 ahead, 4 behind), including KVM nested state restored after the MSRs (6365b4b2, 2de20cdb) and fw_cfg DMA control word decoding (c6dabfbb).
- Isolation: cbsbx was not touched. A private sandboxd b778c66 (cbsbx's binary) ran with its own cocoon root (same XFS, outside the `cbsbx` quota project), net_scope `hb`, `chbench.slice` on CPUs 2-7, restore mode `mmap` (copy-on-write), one `cbdesk:v21` pool (xlarge, 40G, cbsbx's warmup, warm 1) and no cocoond, so the cocoon CLI did the hibernates. 1413535b8 (`01b30417…`, copied from `/srv/cb/sbx/bin`) saved the golden at 03:05:43Z. Then the isolated root's CH was pointed at f1dcd9900, the warm VM still on 1413535b8 was claimed and released, and its refill ran f1dcd9900.
- Bench (as for 8ff614177): one throwaway warm claim, idle 180 s, a 64 MiB random file in `/dev/shm`, then hibernate → wake → sha256 → 60 s idle → hibernate → wake → sha256. Control: the same bench on 1413535b8 in the same root, before the switch. The `b` run had strace attached (60 s idle), so its times are inflated.

| run | gen | hibernate | vm.snapshot | of it, XFS close flush | `memory-ranges` fsync | CH FICLONE | CH written / calls | `write_bytes` delta | new extents (shared) | shared blocks | wake |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1413535b8 | 1 | 0.868 s | 0.735 s | 0.645 s | 25 µs | 77 µs | 272.5 MB / 2,879 | 272,457,728 | 5,757 (2,879) | 96.8% | 0.386 s |
| 1413535b8 | 2 | 0.319 s | 0.243 s | 0.104 s | 19 µs | 111.0 ms | 35.1 MB / 1,432 | 35,106,816 | 7,573 (6,142) | 99.6% | 0.621 s |
| f1dcd9900 a | 1 | 0.969 s | 0.845 s | 0.747 s | 19 µs | 61 µs | 312.7 MB / 2,850 | 312,717,312 | 5,699 (2,850) | 96.4% | 0.493 s |
| f1dcd9900 a | 2 | 0.345 s | 0.268 s | 0.128 s | 21 µs | 106.1 ms | 52.9 MB / 1,636 | 52,879,360 | 7,574 (5,939) | 99.4% | 0.412 s |
| f1dcd9900 a2 | 1 | 0.951 s | 0.809 s | 0.682 s | 18 µs | 63 µs | 286.8 MB / 2,875 | 286,760,960 | 5,749 (2,875) | 96.7% | 0.427 s |
| f1dcd9900 a2 | 2 | 0.299 s | 0.224 s | 0.088 s | 19 µs | 107.1 ms | 34.5 MB / 1,404 | 34,549,760 | 7,566 (6,163) | 99.6% | 0.765 s |
| f1dcd9900 b, strace | 1 | 1.104 s | 0.967 s | 0.701 s | 8.3 ms | 63 µs | 294.5 MB / 2,879 | 294,477,824 | 5,757 (2,879) | 96.6% | 0.438 s |
| f1dcd9900 b, strace | 2 | 0.408 s | 0.313 s | 0.093 s | 16 µs | 107.7 ms | 33.3 MB / 1,417 | 33,288,192 | 7,712 (6,296) | 99.6% | 0.603 s |

- Gen 1's source is the golden's dense `memory-ranges` (2 extents, 0 shared) and gen 2's is the gen-1 file. Extents are `filefrag -v` of the 8 GiB file (2,097,152 blocks). "CH written" is traced write bytes of 4 KiB or more, the snapshot's JSON files included; `/proc/<ch>/io` `write_bytes` is 5.5–6.3 KB higher. "Hibernate" is the sandboxd `POST /hibernate` call, and its split: `vm.info` and `vm.pause` by +0.015 s; `vm.snapshot`, where CH's own writes end by +0.11–0.14 s in gen 1 (+0.15–0.16 s in gen 2, after the 106–111 ms FICLONE; later under strace) and the rest is XFS flushing the file on close (`filemap_flush`, as with 8ff614177); then cocoon's FICLONE of `cow.raw` (12–47 ms), 18 fsyncs (the `memory-ranges` one 16–25 µs, 8.3 ms once under strace; the largest 1.4–27.5 ms) and `vm.shutdown` (28–72 ms).
- Correctness: the tmpfs sha256 matched after every wake. At the end of each run, xorg, session, toolhost and agent were active and `systemctl --failed` was 0.
- Rework checks:
  - pagemap fd: after the copy-on-write restore and before any snapshot, `/proc/<ch>/fd` held exactly one pagemap fd (`34 -> /proc/<pid>/pagemap`, next to `33 ->` the run dir's `memory-ranges`). That was so at the claim, before each hibernate and in each woken CH; 1413535b8 holds none.
  - syscalls: `strace -f -tt -T` attached at the vmm thread (which brings in all 25 threads), over gen 1. The vmm thread opened `config.json`, `state.json` and `memory-ranges` in the new snapshot dir (`O_EXCL`), then `ioctl(295, FICLONE, 33) = 0` (78 µs), then `ftruncate(295, 8589934592)`. Then 6 `pread64(34, …, 1 MiB)` of the pagemap (the 3 GiB range), 45 lseek+write pairs, 10 preads (the 5 GiB range) and 2,833 pairs. The 2,878 `lseek(295)` offsets rise strictly to 8,589,926,400. The traced threads had 3 openat calls in all, and none opened `/proc/self/pagemap` or `/proc/self/maps`. Gen 2 is the same with a 107.7 ms FICLONE and 1,416 pairs. The bpftrace opens agree for every run: 1413535b8 opens `/proc/self/maps` and `/proc/self/pagemap` at each snapshot, f1dcd9900 only the three snapshot files.
  - seccomp: no SIGSYS (strace and a bpftrace `signal_generate` probe for signal 31). The vmm thread has `Seccomp: 2`. Every restore and snapshot answered 204, and the CH threads exited 0 after `vm.shutdown`.
- Landlock: CH f1dcd9900 ran alone in a transient unit on a private reflink copy of the golden. Its `config.json` points the cow disk at the copy, uses a new vsock path and console `Null`, and sets `"landlock_enable": true` and `"landlock_rules": [{"path": "<dst>", "access": "rw"}]`, with dst on the same XFS. `vm.restore` with `memory_restore_mode` `CopyOnWrite` answered 204; the API takes the enum spelling, and `copyonwrite` answers 400. The restored CH held one pagemap fd. After 20 s resumed and a pause, `vm.snapshot` into dst answered 204 in 0.306 s: `write_bytes` +77,729,792, `memory-ranges` 5,820 extents (2,911 shared). Negative control: a second snapshot into a dir with no rule, on the same XFS, failed with `Permission denied (os error 13)` creating `config.json`, so landlock was in force. CH stayed alive, and `vm.shutdown` answered 204.
- Rollback: f1dcd9900 hibernated a claim (5,771 extents), the root went back to 1413535b8, and the wake on 1413535b8 took 0.432 s with the same sha256. 1413535b8 then hibernated that VM through its clone path (0.321 s, 7,609 extents, 6,215 shared) and woke it (same sha256). So a binary swap in either direction keeps hibernated boxes wakeable.
- Against 8ff614177 (hibernate 1 0.83–1.03 s and 265–275 MB, hibernate 2 0.30 s and 32–38 MB), f1dcd9900 is the same: 0.95–0.97 s and 287–313 MB, then 0.30–0.35 s and 35–53 MB. So is 1413535b8 on the same root. The rework moves the pagemap open to the restore; it does not change the I/O.

## Balloon and the cocoon daemon (2026-10-02)

- CH dev 6f08b0859 carries upstream cloud-hypervisor#8963: one balloon `MADV_DONTNEED` per contiguous run instead of per 4 KiB page. Against dev ecaf86344, a standalone A/B on a private copy of the v13 golden (8 runs, ABBA) measured 2–3k balloon madvise calls per clone instead of 440k, about half the clone's idle CPU (17 s instead of 32 s in its first minute) and a guest load during inflation in 19.9 s instead of 43.4 s; the new build restores goldens saved by ecaf86344 (204 in all 4 runs; `smoke cbdesk:v13` cold and `cbdesk:v14` with its refill PASS after the swap).
- A golden saved by the new build does not restore on the old one. The remaining per-clone cost (about 17 CPU-s, 0.5M COW faults, because goldens were saved with the balloon partly inflated) is cocoonstack/cocoon#275; cocoon b239446 (#276) waits for the balloon before the pause, and the v15 golden was rebuilt with it.
- Golden rebuild: the golden stamp holds no cocoon version, so sandboxd would have adopted the old v15 golden. With the old golden moved aside, sandboxd built a new one at boot with b239446 (`golden ready` 06:19:02Z, 30 s after the start); the stamp text is unchanged. A probe polled the build VM's CH `vm.info` every 100 ms (read-only): memory 8 GiB, balloon 2 GiB, so the target `memory_actual_size` is 6 GiB. The balloon inflated at about 0.3 GiB/s from boot; `snapshot save` started with 7.074 GiB actual, cocoon waited 3.43 s, and the VM paused at 6.000 GiB (6442450944 bytes, equal to memory − balloon). No stall warning in the cocoon log. Before #276 this snapshot would have paused about 1.07 GiB short of the target.
- How cbsbx uses cocoon: sandboxd runs the CLI for every operation and follows `cbsbx-cocoond`'s `/v1/events`. Each hypervisor runs in its own `cbsbx.slice/vm-<id>.scope`, outside the daemon's cgroup, so a daemon restart leaves the VMs running and the next daemon adopts them. The daemon serves `/v1/events` before its first reconcile pass, so a sandboxd that reconnects in that window gets an empty sync; it re-checks each claim with `vm inspect` before acting; a cocoon upgrade still keeps sandboxd stopped across the daemon restart.

## Lessons from image cuts

- Fonts: Chrome's bundled fontconfig read stale `*.cache-12` files whose epoch mtimes never looked stale, so CJK rendered blank. Seal drops `/home/user/.cache/fontconfig`.
- SSH host keys (up to v11): an ordering cycle between `openssh-server` and `cocoonbox-hostkeys.service` lost ssh host keys in 2 of 6 v11 boots and failed `systemctl restart ssh`; v12 removed both.
- Egress latency: silkd 6a07d5a sets TCP_NODELAY on the egress and port-relay guest sockets (#281). Kept-alive GETs of one 8 KiB chunk through 127.0.0.1:3128: p50 42.0 ms on v12 (silkd 90230c0), 0.8–1.0 ms on v13 (six runs of 100, max 3.1 ms).
- Large restore (v14, 2026-10-01): 105,000 files of 1 KiB under `/workspace/v14-many` (105,530 unique chunks); SaveState 129.8 s (plan 69.5 s, upload 60 s in 52 batches of at most 2048); then Rebase v14→v14 95.2 s: quiesced save 46.3 s, claim and boot 1.6 s, restore 48 s, of which the GET window was about 30.5 s (105,533 chunks, about 3,500 a second, progress at +47.9 s to the last fetch near +78.4 s), staging writes about 11.5 s, the session stop, swap and start 0.3 s, and agent.db, Chrome and the recover about 4.9 s to DONE. Before v14 a restore of this size was one RestoreRequest of about 49 MB (466 B per chunk), which the agent's 4 MiB default refused.
- Chrome warmup (v15): `cbsbx-conf` gives v15 and later pools a warmup that, after the readiness loop, runs `google-chrome --headless=new --dump-dom about:blank` as `user` in a throwaway profile (`/tmp/cb-warmup-chrome`, removed), so the golden's page cache holds Chrome's binaries and no window or profile. Interleaved A/B on same-image Rebases v15→v15 (6 runs each way, refill at least 60 s old): Chrome's restored window after DONE median 2.28 s (2.05–3.25 s) without, 2.10 s (1.91–2.25 s) with; first `browser_snapshot` through the toolhost answered after DONE median 2.66 s (2.46–3.57 s) without, 1.0 s (0.87–1.16 s) with. With the warmup that first call can come before the restored tab has loaded (title empty); no extra tab is opened (3 page targets), and every run had exactly one Chrome window.
- cua-driver session labels (v17): cua-driver 0.30.4 ends a named session after 300 s idle (its sweep runs every 30 s, so a 315 s idle ends it only about half the time) and keeps it ended until `start_session`; only a transport's unnamed session is recreated on use. A hibernate over 5 min also ends the label, since guest clocks advance across a hibernate (monotonic +340.0 s over a 340 s hibernate).
- cua-driver start (v34): before v34, the autostart restarted the driver about 2.4 s after the session unit had started it, and a driver call in flight then failed with "decode cua-driver reply: EOF". Since v34 the autostart entry is the driver's only start in a session.
- Vault render (v32): a 30+30 key burst left a deleted key rendered, since `EnvironmentFile=` snapshots the file at job start and a rewrite during activation is merged and lost. The render now reads `/run/silkd.env` itself until the file holds still: 40 direct rewrites per trial left it stale in 22/40 trials before, 0/140 after.
- Modal dialogs (v29): an agent that clicks the "Save As..." push button element through its AT-SPI DoAction deadlocks the modal (reproduced identically on v25). Since v30 an element click on a shown push button or menu item is a hit-tested pointer click in a one-shot driver session.
- Test gates: a gates run started with `&` from a non-interactive ssh shell inherited SIGINT as ignored (`SigIgn 0x7`), so the Exec signal test hung on its `trap`; run the gates in the foreground.

## gw notes

Prompt caching on the `openai-chat` wire (2026-10-06): gw moved from the v0.2.7 release to cocoonstack/gateway main 13a3d906 (#148), the only commit on top of v0.2.7. A `/v1/messages` client's `cache_control` breakpoints on system blocks, message blocks and `tool_result` now reach the `openai-chat` wire (OpenRouter) as content parts. Before, gw sent them as plain strings, so a long-prompt turn paid the full input rate on its whole prompt every time.

- Cache acceptance, a second gw tenant, model alias `claude-sonnet-5`:
  - `gw-fidelity.py`, 14/14 PASS (it failed only its cache check on v0.2.7): cache writes 6044/0/0, reads 0/6044/6044. Cost was 16430, 2528 and 2070 micros, against 13408, 13408 and 12950 at the full rate.
  - One agent task through gw (4 model calls, 14 s). The client's upstream log lines show `cache_create` 53251/306/0/0 and `cache_read` 0/53251/53251/53251. The gw ledger cost was 134872, 13070, 12670 and 12426 micros, $0.173 in all. At the full rate these 4 calls would cost $0.434, and before #148 each 53k-token turn paid about 107k micros.
  - Second signals: the client's own session usage (input 1432, cacheRead 159753, cacheWrite 53557) adds up to the gw prompt tokens. In the tenant's `/admin/usage`, which has no cache column, the 4 calls moved prompt plus completion by 215175 but the weighted `total_tokens` by only 84787. That gap is the cache-read rate of 0.1.

Daily counters: without Redis, gw's daily counters are in process, so they restarted with gw and reset every 24 h from the process start, not at UTC midnight. The console's "spent today" is the ledger since UTC midnight.

## New image cut

1. `sudo image/cut.sh [-p] [-a <agent binary>] [-f <family>] [-r <runtime dir>]... cbdesk:vN-1 vN`. `-p` reruns `provision.sh` (only when `image/` changed); `-a` installs an agent binary; each `-r` adds a runtime directory whose `provision.sh` runs in the guest, in order; `-f cbcore` cuts `cbcore:vN` instead, adding `image/desk` when no `-r` is given (its cua-driver tarball goes into `image/desk/vendor/` first) and running `image/clean-gate.sh` in the guest after the agent step. It uses only the cbsbx cocoon CLI and silkd's egress relay: no claim, no api. The script stops at the first failure, keeping the VM for inspection (it prints the `vm rm` line). The steps:
   - `cocoon-cbsbx vm run --name cbcut-vN --nics 0` from `cbdesk:vN-1` (a cold boot, as the sandboxd golden does), then wait for Xorg and the session;
   - with `-p`, serve `image/cut-proxy.py` (an HTTP forward proxy, stdlib only, no allowlist) on the VM's `<vsock_socket>_2049` unix socket only, where the guest's silkd relays `127.0.0.1:3128`; copy `provision.sh` and `rootfs/` in and run provision as root through `vm exec` with `http(s)_proxy` set (apt has its own proxy conf); the trap stops the proxy;
   - with `-a`, install the agent and enable `cocoonbox-agent`, run each runtime's `after-agent.sh`, and require the toolhost's shuttle service spec `/etc/cocoonbox/tools.d/cocoonbox.json`;
   - `seal.sh` through `vm exec`, then `vm stop`;
   - `image/export-rootfs.sh` on the stopped VM: it reads its layers and cow from `vm inspect` and overlays silkd, the proxy drop-ins, masked wait-online, an apt proxy conf, and a static `agent.env` (`GW_API_KEY=unprovisioned`, `AGENT_LISTEN=127.0.0.1:7443`, no `EDGE_TOKEN`; the export fails if the edge token would be in it); then `vm rm`;
   - `cocoon-cbsbx image import cbdesk:vN /srv/cb/sbx/build/cocoonbox-desktop-vN.tar.gz`.
2. Pools are API-owned since 2026-10-01 17:50Z: `/srv/cb/sbx/data/pools.json` (written by the first `PUT /v1/pools`) seeds the warm targets at boot and overrides the `pools` warm values in `sandboxd.json`; deleting it returns them to the config. A pool's egress, warmup, storage and capture_trim stay config-owned and load only at boot, so a new template key needs one restart today (cocoonstack/sandbox#288 asks for a runtime reload):
   - `sudo cbsbx-conf cbdesk:vN-1=1 cbdesk:vN=0 …` with every template that is still pooled, so `sandboxd.json` carries vN's egress, warmup and 40G storage (its warm value no longer matters), then one `sudo systemctl restart cbsbx-sandboxd`, which keeps running claims;
   - then switch the warm targets online with `PUT /v1/pools` (root token, from `/etc/cocoonbox/sandboxd-token`), naming every pool to keep, since an omitted pool is drained: `{"pools": [{"template": "cbdesk:vN", "net": "none", "size": "xlarge", "warm": 1}, {"template": "cbdesk:vN-1", "net": "none", "size": "xlarge", "warm": 0}, …]}`. It answers the new `GET /v1/info` and needs no restart; no claim's VM is touched. The body must not carry egress, warmup or storage (400). The golden builds within about 30 s of the restart even at warm 0; `GET /v1/info` then shows `golden: true`, `warm: 1`.
3. `sudo deploy/cbsbx/smoke cbdesk:vN`, which must print `PASS`. It uses one claim (no `cb_machine` metadata) and a throwaway gw key, generated by gw's admin API with the tenant token and deleted by its `ak_id`; both are removed at the end. It checks: guest units, the 40 GiB root, gw 200 through the claim secret, the store and the internet through the proxy, 10.203.0.1 :10260/:10250/:6443/:22 refused by the proxy (502; the `CB-OUTPUT` reset counter does not move), no key in the guest's environ, `/etc` or `/run`, `GET env` hiding it, the agent answering through the relay, and gw 401 after `PATCH env` removes `GW_KEY`.
4. Set `image: cbdesk:vN` in `/etc/cocoonbox/api.yaml` (the previous one into `rebase_images` if it is v14 or later; older agents cannot take the paged Restore) and restart the api.

Proof, 2026-10-01: `cut.sh -p cbdesk:v11 v11b` with no change to `image/`: boot 5 s, provision 30 s (apt and the Chrome and cua-driver downloads through `cut-proxy.py`), seal 3 s, export 38 s, import 55 s, 682 MB. Against v11's tarball: the same dpkg package set (Chrome 154.0.8037.92-1), no ssh host keys, one mode change (`xfwm4.xml`, reinstalled by provision), and otherwise only runtime churn (journal, `/tmp`, `random-seed`, the agent's WAL). `deploy/cbsbx/smoke cbdesk:v11b`: PASS. The template, its golden and its tarball were deleted afterwards. The first run lost three apt `InRelease` files: the guest's overlay (`metacopy=on`) copies metadata-only changes up as metacopy entries, and the export mounted without `metacopy`, so those files vanished from the tarball. The export now mounts with `metacopy=on`; the v11 golden's cow held one such entry (`/var/lib/dpkg/diversions-old`), so earlier templates lost only that backup file.
- sandboxd keeps the golden of a removed pool (`/srv/cb/sbx/data/goldens/<hash>`, ~8 GB each); delete it by hand after dropping a pool for good. The stamp file next to it names the image.

Safety rule for every command on a node: each `sandboxd` or `cocoon` invocation carries an explicit `-config` (`--config` for cocoon) or goes through `cocoon-cbsbx`. Check a version with `sandboxd -version` or `go version -m`, never with a bare binary or an unknown argument, and never pipe into `head` a command that can still act. sandboxd ignores positional arguments and starts with the default `/etc/sandboxd/config.json`, and its startup reconcile removes every `sbx-` VM no claim of that config owns. On 2026-10-01 a `sandboxd version` run this way removed two warm VMs of the host's other sandboxd instance before `head -3` closed its pipe.

## Restoring a box from the store

A Rebase always saves the current machine first, then restores that save. Never Rebase a freshly created, empty machine to restore a box: the Rebase saves the empty state and restores it. Use restore-on-missing instead (api ≥ fda6d5d). When a box has no machine, Ensure creates one and restores the box's head: `PHASE_RESTORING`, then "restored snapshot <id>". The new machine starts from the latest image of the family recorded in the head (`image`, since the 2026-10-07 api); a head without it, such as a hand-written one, restores onto the default image.
- Free a box's machine for a window: run SaveState and check the new head. Then release that machine's claim (`POST http://127.0.0.1:17780/v1/sandboxes/{id}/release` with the root token); its gw key is the user's and stays. There is no Delete RPC yet. Ensure brings the box back from that head.
- Restore a snapshot older than the head: release the machine first, so no scheduled save moves the head. Back up `<box>/head.json` and rewrite it to `{"latest": "<id>", "saved_at_ms": <its saved_at_ms>, "image": "<the old head's image>"}`. Restart the api, because it caches heads, then run Ensure. Since 2026-10-02 the GC keeps only the snapshots of the retention rule (design §25.3), so pick the id from the box's `timeline.json`; a head that is not in it stops the user's GC pass with an error.

## Deploying a binary into a box

Pipe the binary into the box's VM as root over vsock. The VM name is the claim's `vm_name` (`GET /v1/sandboxes/{id}` with the root token):

```bash
sudo /srv/cb/sbx/bin/cocoon-cbsbx vm exec -i <vm> -- sh -c 'cat > /tmp/cocoonbox-agent' < target/release/cocoonbox-agent
sudo /srv/cb/sbx/bin/cocoon-cbsbx vm exec <vm> -- sh -c 'install -m 0755 /tmp/cocoonbox-agent /usr/local/bin/ && systemctl restart cocoonbox-agent && systemctl --user -M user@ restart shuttle'
```

A change made this way lasts until the box's next Rebase; ship it in the next template.

## Switch to the sandboxd backend (M4.8, 2026-09-30)

Every provisioned user moved from CocoonSets to claims on the cbsbx instance (design §14): a quiesced SaveState on cocoonset, the api flipped to `backend: sandboxd`, then `Ensure` restored each box through restore-on-missing. Restore into the new claim: boot 1 s, restore 2 s; round trip on the new backend: hibernate 23 s, wake op DONE in 1.0 s.

On the old backend, the hibernate push spooled 8 GiB to `/tmp` on the root fs; the kubelet evicted the registry and cocoon-operator (DiskPressure) and the hibernate timed out. With `TMPDIR` on the NVMe the next hibernate took 106 s with the root fs flat.

Two defects surfaced on hardware; unit tests missed both:
- MCP hang: the sandboxd endpoint key was `sandboxd:<id>`, no host:port. `edge/mcp.go` puts the key into the upstream URL host, and net/http then never found the connection it had dialed and redialed until the client gave up. gRPC was unaffected. Fix `fbae11c`: key `<id>.sandboxd:<agent port>`.
- Wake operations timed out on a ready box: the sandboxd `Watch` deduplicated on state, reason, incarnation and revision, so the provisioner's `SetMeta` of the wake id sent no update and the operation waited 10 min. Fix `64415a1`: the dedup key includes `Meta`, as `Watch`'s contract (an update on every change) requires.

## Known gaps

- Claims keep the golden's hostname, because they are memory clones of it (and, up to v11, its SSH host keys).
- The `anthropic/` models reach OpenRouter through gw's messages→chat translation (the `openrouter` kind speaks only `openai-chat`).
