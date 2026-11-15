// Science results for /science/tauceti/ and the home page: science/tauceti.json, exported from our
// own run records by an export script (public fields only) and committed here. Tau Ceti is a
// finished project (2026-11-15): this is the final data, frozen from the gateway's last daily
// export, what the build renders and the home page counts, and what /science/tauceti.json serves.
// Nothing is loaded live.
// normalize() drops anything malformed, so the page shows only what the data really says.
// The model code is in science-model.ts.
import raw from './science/tauceti.json';
import { normalize } from './science-model';

export * from './science-model';
export const science = normalize(raw);
