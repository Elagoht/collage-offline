// A collage plugin that serves a service worker, so the pages a visitor has read
// open again without a network: HTML network-first, static files
// stale-while-revalidate, and a fallback page for everything else.
//
// It requires collage the way any consumer does, and reaches nothing the framework
// does not offer every plugin.
module github.com/Elagoht/collage-offline

go 1.26

require github.com/Elagoht/collage v0.50.0

retract v0.1.4 // tagged at v0.1.3's commit by mistake
