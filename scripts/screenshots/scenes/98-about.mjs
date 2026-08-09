// The About box: what this is, what it is driving, and where the source is.

export default {
  name: "about",
  description: "version, licence and links",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await page.click("#btn-about");
    await page.waitFor("#about:not(.is-hidden)", { what: "the about box" });

    // The version and the gdb behind it are filled in when the box opens, so
    // an empty field here means the snapshot never reached it.
    await page.waitUntil(
      () => {
        const gdb = document.querySelector("#about-gdb")?.textContent ?? "";
        return document.querySelector("#about-version")?.textContent
          && gdb && gdb !== "not started";
      },
      { what: "the version and the gdb to be filled in" },
    );
    // The avatar ships in the binary. A broken image would still lay out, so
    // this asks the image itself whether any pixels arrived.
    await page.waitUntil(
      () => {
        const img = document.querySelector(".about-avatar");
        return img?.complete && img.naturalWidth > 0;
      },
      { what: "the avatar to load" },
    );

    await page.shot(ctx.image(), { clip: ".about", pad: 12 });

    // Escape shuts it. The keymap runs in the capture phase and takes some
    // keys before anything else sees them, so whether this one arrives is a
    // question about the keymap rather than about this box.
    await page.key("Escape");
    await page.waitFor("#about.is-hidden", { what: "Escape to close the box" });
  },
};
