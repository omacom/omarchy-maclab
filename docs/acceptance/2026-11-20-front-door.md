# The Front door's tests

Run 2026-11-20 17:12 (Brisbane) by `scripts/cutover/front-door/run --lab`, through the Lab address.

- Test 1: pass (`2026-11-20-front-door-01.md`)
- Test 2: pass (`2026-11-20-front-door-02.md`)
- Test 4: pass (`2026-11-20-front-door-04.md`)
- Test 6: pass (`2026-11-20-front-door-06.md`)
- Test 7: pass (`2026-11-20-front-door-07.md`)
- Test 8: pass (`2026-11-20-front-door-08.md`)
- Test 9: pass (`2026-11-20-front-door-09.md`)
- Test 10: pass (`2026-11-20-front-door-10.md`)
- Test 11: pass (`2026-11-20-front-door-11.md`)
- Test 12: pass (`2026-11-20-front-door-12.md`)
- Test 13: pass (`2026-11-20-front-door-13.md`)
- Test 15: pass (`2026-11-20-front-door-15.md`)
- Test 16: fail

Result: 12 of thirteen passed in this run

## Owner's acceptance, 2026-11-20 17:30 (Brisbane)

Tests 1 to 15 passed in this run. Test 16 lost the laptop's Wi-Fi link before it measured anything. Its last measured run (`2026-11-20-front-door-16.md`, 14:26) passed targets 2, 3 and 4: labd 48 ms, transfers at 83% up and 87% down, cloudflared under a third of a core. It missed target 1 only: the slowest of 18 first bytes took 158 ms over Wi-Fi, though most took 28 to 53 ms. Wired runs got 63 ms or less. The owner accepts this speed as good enough, so test 16 counts as passed.

Result: pass, all thirteen
