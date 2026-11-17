# Both Lab-owned Macs accepted, 2026-11-16

Ready 6. Accepted in Preview (spec, "Moving the fleet"): checked in, baseline passed, one job run, sighted on its port or placed by a Proving reset, then one Hard reset. The Machine key sign-in is proved after Live.

- m2-max (MacBook Pro 16-inch M2 Max): Joined Lab-owned 13:02 after the Volunteer phase (step 14a); baseline j1116-130909-19fb passed; placed on the middle port by a Proving reset (18:10, kept across restarts); job at 20:17 passed; Hard reset 20:19 through the middle port, back up 20:19:37.
- m1-pro (MacBook Pro 14-inch M1 Pro): left Chris's lab (its agent's uninstall told that lab, which lists m1pro as left), Joined Lab-owned 19:4x; baseline j1116-194853-74ee passed; sighted on the right port by name; job at 20:14 passed; first Hard reset refused because a generic Thunderbolt name during boot dropped the Sighting (fixed separately), second Hard reset 20:19:48 through the right port, back up 20:20:08.

```
== 2026-11-16 20:13:35 AEST m1-pro
m1-pro  READY
  model:       Apple MacBook Pro (14-inch, M1 Pro, 2021), MacBook Pro 14 M1 Pro, at the mini, right port
-- one job
  20:14:21  booted 7.2.11-14-1-ARCH in 34s
  20:14:23  test boot-health: pass (0s)
  20:15:52  omarchy-m-test 0.2.0-rc.6: 51 pass, 4 fail, 31 skipped; 0 regressed, 0 fixed vs the previous known-good run j1116-194853-74ee; coverage not run: video.h264-on-screen, video.hevc-on-screen
  20:15:52  test omarchy-m-test: pass (89s)
-- one Hard reset
lab: no Hard reset was sent: the lab has no Sighting of m1-pro (its Sighting was dropped at Mon 20:16 AEST: the right port shows the name "unknown_xd_device", which is not its own). A Maintainer can prove its port: `lab reset m1-pro --prove left|right`
m1-pro: sighted on the right port
  since:       2026-11-16 20:16:43
  hard reset:  through the right port (`lab reset m1-pro`)
  model:       Apple MacBook Pro (14-inch, M1 Pro, 2021), MacBook Pro 14 M1 Pro, at the mini, right port
== 2026-11-16 20:16:51 AEST m2-max
m2-max  READY
  model:       Apple MacBook Pro (16-inch, M2 Max, 2023), MacBook Pro 16 M2 Max, at the mini, port 0
-- one job
  20:17:31  booted 7.2.11-15-1-ARCH in 31s
  20:17:33  test boot-health: pass (0s)
  20:18:43  omarchy-m-test 0.2.0-rc.3: 49 pass, 8 fail, 29 skipped; 0 regressed, 0 fixed vs the previous known-good run j1116-163806-7cb1; coverage not run: video.h264-on-screen, video.hevc-on-screen
  20:18:43  test omarchy-m-test: pass (70s)
-- one Hard reset
Hard reset sent to m2-max through the middle port; watch `lab status m2-max`
m2-max: sighted on the middle port by a Proving reset
  since:       2026-11-16 18:10:00
  hard reset:  through the middle port (`lab reset m2-max`)
  model:       Apple MacBook Pro (16-inch, M2 Max, 2023), MacBook Pro 16 M2 Max, at the mini, port 0
== 2026-11-16 20:19:48 AEST m1-pro Hard reset again (sighted since 20:16:43)
Hard reset sent to m1-pro through the right port; watch `lab status m1-pro`
m1-pro  READY
m1-pro: sighted on the right port
  since:       2026-11-16 20:16:43
== m2-max after its Hard reset
m2-max  READY
2026-11-16 20:19:37
2026-11-16 20:20:08
```

Result: pass
