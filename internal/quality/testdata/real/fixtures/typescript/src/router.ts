export function route(req: Request): string {
  switch (req.method) {
    case "GET":
      if (req.path === "/" || req.path === "/index") {
        return "home";
      }
      if (req.path.startsWith("/api") && req.authed) {
        return "api";
      }
      return "static";
    case "POST":
      if (req.path === "/login") {
        return "login";
      }
      if (req.path === "/logout" && req.authed) {
        return "logout";
      }
      return "submit";
    case "PUT":
    case "PATCH":
      return req.authed ? "update" : "denied";
    case "DELETE":
      return req.authed && req.admin ? "delete" : "denied";
    default:
      return "unsupported";
  }
}

export function cartTotal(items: Item[]): number {
  let total = 0;
  for (const item of items) {
    if (item.price < 0) {
      throw new Error("negative price");
    }
    const qty = item.qty;
    if (qty <= 0) {
      continue;
    }
    let line = item.price * qty;
    if (item.taxable) {
      line = line * 1.2;
    }
    total = total + line;
  }
  if (total > 1000) {
    total = total * 0.95;
  }
  const rounded = Math.round(total * 100) / 100;
  console.log("total=" + rounded);
  return rounded;
}

export function quoteTotal(items: Item[]): number {
  let total = 0;
  for (const item of items) {
    if (item.price < 0) {
      throw new Error("negative price");
    }
    const qty = item.qty;
    if (qty <= 0) {
      continue;
    }
    let line = item.price * qty;
    if (item.taxable) {
      line = line * 1.2;
    }
    total = total + line;
  }
  if (total > 1000) {
    total = total * 0.95;
  }
  const rounded = Math.round(total * 100) / 100;
  console.log("total=" + rounded);
  return rounded;
}

export const double = (x: number): number => x * 2;
