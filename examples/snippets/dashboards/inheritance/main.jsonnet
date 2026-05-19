local base = {
  meta:: { version: "1.0" },
  name: "base",
  tags: ["a"],
  count:: std.length(self.tags),
  description: "Has " + self.count + " tag(s)",
};

base + {
  name: "extended",
  tags+: ["b", "c"],
}
