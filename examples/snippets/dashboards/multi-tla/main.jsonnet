function(tags=["default"])
  {
    count: std.length(tags),
    list: tags,
    joined: std.join(", ", tags),
  }
