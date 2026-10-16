require ["fileinto", "copy", "imap4flags"];

# Apply to unprocessed mail in filter_folders, including the first sync.
if header :contains "from" "builds@example.com" {
  addflag "\\Seen";
  fileinto "INBOX/Builds";
  stop;
}

if header :contains ["to", "cc"] "reviews@example.com" {
  fileinto :copy "INBOX/Review/Mine";
  fileinto "INBOX/Review";
  stop;
}
