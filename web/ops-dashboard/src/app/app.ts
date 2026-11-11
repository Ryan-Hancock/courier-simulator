import {
  AfterViewInit,
  Component,
  ElementRef,
  OnDestroy,
  computed,
  effect,
  inject,
  signal,
  viewChild,
} from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { firstValueFrom } from 'rxjs';
import * as L from 'leaflet';

interface Driver {
  driver_id: number;
  name: string;
  city: CityKey;
  vehicle: string | null;
  vehicle_type: 'van' | 'ebike' | null;
  is_electric: boolean | null;
}

interface Position {
  driver_id: number;
  at: string;
  lat: number;
  lon: number;
  speed_kmh: number | null;
  battery_pct: number | null;
}

interface Cell {
  lat: number;
  lon: number;
  count: number;
}

type CityKey = 'glasgow' | 'edinburgh';
type Kind = 'diesel' | 'ev' | 'ebike';

const CITIES: Record<CityKey, { name: string; centre: L.LatLngTuple }> = {
  glasgow: { name: 'Glasgow', centre: [55.8611, -4.2502] },
  edinburgh: { name: 'Edinburgh', centre: [55.952, -3.1895] },
};

const KINDS: Record<Kind, { label: string; colour: string }> = {
  diesel: { label: 'Diesel van', colour: 'var(--diesel)' },
  ev: { label: 'Electric van', colour: 'var(--ev)' },
  ebike: { label: 'E-cargo bike', colour: 'var(--ebike)' },
};

const TRACKING = 'api/tracking/v1';
const TICK_MS = 1000;

const clock = new Intl.DateTimeFormat('en-GB', {
  timeZone: 'Europe/London',
  weekday: 'short',
  day: 'numeric',
  month: 'short',
  year: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
});

function kindOf(d: Driver | undefined): Kind {
  if (d?.vehicle_type === 'ebike') return 'ebike';
  return d?.is_electric ? 'ev' : 'diesel';
}

@Component({
  selector: 'app-root',
  template: `
    <header>
      <div class="brand">
        <span class="logo">GekGo</span>
        <span class="sub">Ops · <em>fictional company, simulated data</em></span>
      </div>
      <nav class="cities" aria-label="City">
        @for (c of cityKeys; track c) {
          <button [class.on]="city() === c" (click)="city.set(c)">{{ cities[c].name }}</button>
        }
      </nav>
    </header>

    <main>
      <div #map class="map" role="application" aria-label="Live map of GekGo vehicles"></div>

      <aside>
        <section class="stats">
          <div><strong>{{ stats().active }}</strong><span>on the road</span></div>
          <div><strong>{{ stats().moving }}</strong><span>moving</span></div>
          <div>
            <strong>{{ stats().battery ?? '–' }}{{ stats().battery !== null ? '%' : '' }}</strong>
            <span>avg EV battery</span>
          </div>
        </section>

        <section>
          <h2>Fleet</h2>
          <ul class="legend">
            @for (k of kindKeys; track k) {
              <li>
                <i [style.background]="kinds[k].colour"></i>{{ kinds[k].label }}
                <b>{{ stats().byKind[k] }}</b>
              </li>
            }
          </ul>
        </section>

        <section>
          <label class="toggle">
            <input type="checkbox" [checked]="showHistory()" (change)="showHistory.set(!showHistory())" />
            Delivery history
          </label>
          <p class="note">Each dot is a ~250 m square; bigger means more parcels delivered there.</p>
        </section>

        @if (error()) {
          <p class="error" role="alert">{{ error() }}</p>
        } @else if (stats().active === 0 && range()) {
          <p class="note">No vehicles at this moment. Drivers work roughly 07:00–19:30.</p>
        }
      </aside>
    </main>

    <footer>
      <button class="play" (click)="playing.set(!playing())" [attr.aria-label]="playing() ? 'Pause' : 'Play'">
        {{ playing() ? '❚❚' : '▶' }}
      </button>
      <select (change)="speed.set(+$any($event.target).value)" aria-label="Replay speed">
        @for (s of speeds; track s) {
          <option [value]="s" [selected]="s === speed()">{{ s }}×</option>
        }
      </select>
      <input
        type="range"
        aria-label="Replay time"
        [min]="range()?.from ?? 0"
        [max]="range()?.to ?? 0"
        step="60000"
        [value]="at()"
        (input)="seek(+$any($event.target).value)"
      />
      <time>{{ atLabel() }}</time>
    </footer>
  `,
})
export class App implements AfterViewInit, OnDestroy {
  private http = inject(HttpClient);
  private mapEl = viewChild.required<ElementRef<HTMLDivElement>>('map');

  protected readonly cities = CITIES;
  protected readonly cityKeys = Object.keys(CITIES) as CityKey[];
  protected readonly kinds = KINDS;
  protected readonly kindKeys = Object.keys(KINDS) as Kind[];
  protected readonly speeds = [1, 10, 60, 300];

  protected city = signal<CityKey>('glasgow');
  protected range = signal<{ from: number; to: number } | null>(null);
  protected at = signal(0);
  protected playing = signal(true);
  protected speed = signal(60);
  protected showHistory = signal(true);
  protected error = signal<string | null>(null);

  private positions = signal<Position[]>([]);
  private cells = signal<Cell[]>([]);
  private drivers = signal(new Map<number, Driver>());

  private map?: L.Map;
  private markers = new Map<number, L.CircleMarker>();
  private vanLayer = L.layerGroup();
  private historyLayer = L.layerGroup();
  private timer?: ReturnType<typeof setInterval>;
  private inFlight = false;

  protected atLabel = computed(() => (this.at() ? clock.format(this.at()) : 'Loading…'));

  private visible = computed(() => {
    const ds = this.drivers();
    return this.positions().filter((p) => ds.get(p.driver_id)?.city === this.city());
  });

  protected stats = computed(() => {
    const ds = this.drivers();
    const ps = this.visible();
    const byKind: Record<Kind, number> = { diesel: 0, ev: 0, ebike: 0 };
    let batterySum = 0;
    let batteryN = 0;
    for (const p of ps) {
      byKind[kindOf(ds.get(p.driver_id))]++;
      if (p.battery_pct !== null) {
        batterySum += p.battery_pct;
        batteryN++;
      }
    }
    return {
      active: ps.length,
      moving: ps.filter((p) => (p.speed_kmh ?? 0) > 3).length,
      battery: batteryN ? Math.round(batterySum / batteryN) : null,
      byKind,
    };
  });

  constructor() {
    effect(() => this.drawVans(this.visible()));
    effect(() => this.drawHistory(this.cells(), this.showHistory()));
    effect(() => this.map?.setView(CITIES[this.city()].centre, 12));
  }

  async ngAfterViewInit() {
    const canvas = L.canvas({ padding: 0.5 });
    this.map = L.map(this.mapEl().nativeElement, { preferCanvas: true, renderer: canvas, zoomControl: true }).setView(
      CITIES[this.city()].centre,
      12,
    );
    L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png', {
      maxZoom: 18,
      attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
    }).addTo(this.map);
    this.historyLayer.addTo(this.map);
    this.vanLayer.addTo(this.map);

    try {
      const [fleet, range, heat] = await Promise.all([
        firstValueFrom(this.http.get<{ drivers: Driver[] }>(`${TRACKING}/fleet`)),
        firstValueFrom(this.http.get<{ from: string | null; to: string | null }>(`${TRACKING}/pings/range`)),
        firstValueFrom(this.http.get<{ cells: Cell[] }>(`${TRACKING}/history/deliveries`)),
      ]);
      this.drivers.set(new Map(fleet.drivers.map((d) => [d.driver_id, d])));
      this.cells.set(heat.cells);
      if (range.from && range.to) {
        const r = { from: Date.parse(range.from), to: Date.parse(range.to) };
        this.range.set(r);
        this.at.set(Math.max(r.from, r.to - 3 * 3600_000)); // late afternoon on the last day
      }
      await this.fetchPositions();
    } catch {
      this.error.set('Cannot reach tracking-ingest. Is `make up` running?');
    }
    this.timer = setInterval(() => this.tick(), TICK_MS);
  }

  ngOnDestroy() {
    clearInterval(this.timer);
    this.map?.remove();
  }

  protected seek(ms: number) {
    this.at.set(ms);
    this.fetchPositions();
  }

  private tick() {
    const r = this.range();
    if (!this.playing() || !r) return;
    let next = this.at() + this.speed() * TICK_MS;
    if (next > r.to) next = r.from; // loop the replay
    this.at.set(next);
    this.fetchPositions();
  }

  private async fetchPositions() {
    if (this.inFlight || !this.at()) return; // skip a beat rather than pile up requests
    this.inFlight = true;
    try {
      const iso = new Date(this.at()).toISOString();
      const res = await firstValueFrom(
        this.http.get<{ positions: Position[] }>(`${TRACKING}/positions`, { params: { at: iso } }),
      );
      this.positions.set(res.positions);
      this.error.set(null);
    } catch {
      this.error.set('Lost contact with tracking-ingest.');
    } finally {
      this.inFlight = false;
    }
  }

  private drawVans(ps: Position[]) {
    const seen = new Set<number>();
    const ds = this.drivers();
    for (const p of ps) {
      seen.add(p.driver_id);
      const d = ds.get(p.driver_id);
      const colour = getComputedStyle(document.documentElement).getPropertyValue(
        `--${kindOf(d)}`,
      ).trim();
      const tip = `<b>${d?.name ?? 'Driver ' + p.driver_id}</b><br>${d?.vehicle ?? ''} · ${KINDS[kindOf(d)].label}<br>${Math.round(p.speed_kmh ?? 0)} km/h${p.battery_pct !== null ? ' · ' + Math.round(p.battery_pct) + '% battery' : ''}`;
      let m = this.markers.get(p.driver_id);
      if (!m) {
        m = L.circleMarker([p.lat, p.lon], { radius: 6, weight: 2, color: '#fff', fillColor: colour, fillOpacity: 1 });
        m.bindTooltip(tip);
        this.vanLayer.addLayer(m);
        this.markers.set(p.driver_id, m);
      } else {
        m.setLatLng([p.lat, p.lon]);
        m.setTooltipContent(tip);
      }
    }
    for (const [id, m] of this.markers) {
      if (!seen.has(id)) {
        this.vanLayer.removeLayer(m);
        this.markers.delete(id);
      }
    }
  }

  private drawHistory(cells: Cell[], show: boolean) {
    this.historyLayer.clearLayers();
    if (!show || !cells.length) return;
    const max = cells[0].count;
    const colour = getComputedStyle(document.documentElement).getPropertyValue('--history').trim();
    for (const c of cells) {
      const share = Math.sqrt(c.count / max);
      L.circleMarker([c.lat, c.lon], {
        radius: 1.5 + 6 * share,
        stroke: false,
        fillColor: colour,
        fillOpacity: 0.12 + 0.5 * share,
        interactive: false,
      }).addTo(this.historyLayer);
    }
  }
}
