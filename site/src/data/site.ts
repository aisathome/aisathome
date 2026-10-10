// The domain the site is built for: PUBLIC_DOMAIN at build time (the deploy sets it per
// environment, e.g. staging.aisathome.org), otherwise production.
export const domain: string = import.meta.env.PUBLIC_DOMAIN || 'aisathome.org';
export const chatURL = `https://chat.${domain}`;
export const accountURL = `https://${domain}/account`;
