// Science results for /science/tauceti/ and the home page. Two sources with one shape:
//  - science/tauceti.json, exported from our own run records by an export script (public fields
//    only) and committed here: what the build renders and the home page counts, as old as the
//    last export. It is not served: the only published data is the gateway's;
//  - the gateway's GET /v1/projects/tauceti/science: the same data, rebuilt once a day by the
//    runner, plus `now` (what the runner is working on). The page loads it in the browser and
//    renders it with the same code (science-insights.ts) in place of the build's.
// normalize() drops anything malformed, so the page shows only what the data really says.
// The model code is in science-model.ts.
import raw from './science/tauceti.json';
import { normalize } from './science-model';

export * from './science-model';
export const science = normalize(raw);
